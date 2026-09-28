package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func TestManagedActivationRunsInsideUnifiedRedemptionAndSubmitsOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openPublicHandlerTestDB(t, true)
	if err := db.InitManagedActivation(); err != nil {
		t.Fatal(err)
	}

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/cdk/preview" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"CDK不存在"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer primary.Close()

	var creates atomic.Int32
	managed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/recharge/verify-cdk":
			_, _ = w.Write([]byte(`{"valid":true,"plan_type":"plus"}`))
		case "/api/v1/recharge/check-subscription":
			_, _ = w.Write([]byte(`{"ok":true,"summary":{"account_email":"member@example.com","plan_type":"free","has_active_subscription":false,"will_renew":false}}`))
		case "/api/v1/recharge/create-task":
			creates.Add(1)
			_, _ = w.Write([]byte(`{"task_id":"TASK-MANAGED-1","status":"submitted","message":"accepted"}`))
		case "/api/v1/lookup/task":
			_, _ = w.Write([]byte(`{"task_id":"TASK-MANAGED-1","plan_type":"plus","account_email":"member@example.com","task_status":"completed","created_at":"2026-09-28 10:00:00","updated_at":"2026-09-28 10:01:00","completed_at":"2026-09-28 10:01:00"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer managed.Close()
	t.Setenv("CARD_API_BASE", primary.URL)
	t.Setenv("JZ_ACTIVATION_API_BASE", managed.URL)
	t.Setenv("JZ_ACTIVATION_ENABLED", "true")

	router := gin.New()
	router.POST("/preview", PublicCDKPreview)
	router.POST("/preflight", PublicCDKPreflight)
	router.POST("/redeem", PublicCDKRedeem)
	router.POST("/result", PublicCDKResultByCode)

	code := "PLUS-MANAGED-TEST-0001"
	_, preview := callPublicHandler(t, router, "/preview", map[string]any{"code": code})
	redemptionToken := strAny(preview["redemption_token"])
	attemptToken := strAny(preview["attempt_token"])
	if redemptionToken == "" || attemptToken == "" || preview["flow"] != managedFlowName {
		t.Fatalf("preview=%#v", preview)
	}
	session := `{"accessToken":"access","sessionToken":"session","user":{"email":"member@example.com"}}`
	_, preflight := callPublicHandler(t, router, "/preflight", map[string]any{
		"code": code, "redemption_token": redemptionToken, "attempt_token": attemptToken,
		"credential": map[string]any{"mode": "session", "session": session},
	})
	preflightToken := strAny(preflight["preflight_token"])
	if preflightToken == "" || preflight["flow"] != managedFlowName {
		t.Fatalf("preflight=%#v", preflight)
	}

	redeemBody := map[string]any{
		"code": code, "redemption_token": redemptionToken, "attempt_token": attemptToken,
		"preflight_token": preflightToken,
		"credential":      map[string]any{"mode": "session", "session": session},
	}
	recorder, redeem := recordPublicHandler(t, router, "/redeem", redeemBody)
	if recorder.Code != http.StatusAccepted || redeem["status"] != "submitted" || creates.Load() != 1 {
		t.Fatalf("redeem status=%d body=%#v creates=%d", recorder.Code, redeem, creates.Load())
	}
	recorder, duplicate := recordPublicHandler(t, router, "/redeem", redeemBody)
	if recorder.Code != http.StatusConflict || creates.Load() != 1 {
		t.Fatalf("duplicate status=%d body=%#v creates=%d", recorder.Code, duplicate, creates.Load())
	}

	status, result := callPublicHandler(t, router, "/result", map[string]any{"code": code})
	if status != http.StatusOK || result["status"] != "completed" || result["flow"] != managedFlowName {
		raw, _ := json.Marshal(result)
		t.Fatalf("result status=%d body=%s", status, raw)
	}
	lookup, ok := lookupManagedCDK(t.Context(), code, time.Now())
	if !ok || lookup.Status != "used" || lookup.AccountEmail != "m****r@example.com" {
		raw, _ := json.Marshal(lookup)
		t.Fatalf("lookup ok=%t body=%s", ok, raw)
	}
}

func TestManagedFallbackOnlyForUnknownPrimaryCode(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"not found", http.StatusNotFound, `{}`, true},
		{"explicit missing", http.StatusBadRequest, `{"error":"CDK不存在"}`, true},
		{"used primary code", http.StatusBadRequest, `{"error":"CDK已被使用"}`, false},
		{"disabled primary code", http.StatusBadRequest, `{"error":"CDK已禁用"}`, false},
		{"primary outage", http.StatusBadGateway, `{"error":"upstream unavailable"}`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := primaryAllowsManagedFallback(test.status, []byte(test.body)); got != test.want {
				t.Fatalf("got %t, want %t", got, test.want)
			}
		})
	}
}
