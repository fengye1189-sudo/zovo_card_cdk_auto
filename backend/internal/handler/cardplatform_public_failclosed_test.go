package handler

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func openPublicHandlerTestDB(t *testing.T, withBindings bool) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "public-handler.db")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	previous := db.DB
	db.DB = database
	t.Cleanup(func() {
		_ = database.Close()
		db.DB = previous
	})
	if !withBindings {
		return database
	}
	if _, err := database.Exec(`
		CREATE TABLE site_settings (
			key TEXT PRIMARY KEY, value TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE cdk_session_bindings (
			cdk_code TEXT PRIMARY KEY,
			session_payload TEXT NOT NULL DEFAULT '',
			redemption_token TEXT,
			account_email TEXT NOT NULL DEFAULT '',
			attempt_nonce TEXT NOT NULL DEFAULT '',
			submit_claimed_at INTEGER NOT NULL DEFAULT 0,
			query_started_at INTEGER NOT NULL DEFAULT 0,
			query_expires_at INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE recharge_tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id TEXT UNIQUE,
			cdk_code TEXT NOT NULL,
			session_json TEXT,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE cdk_preflight_grants (
			attempt_nonce TEXT NOT NULL,
			token_hash TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY(attempt_nonce, token_hash)
		);
		CREATE TABLE card_selection_rules (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			sort_order INTEGER NOT NULL DEFAULT 0,
			plan_key TEXT NOT NULL,
			display_name TEXT NOT NULL,
			bin_prefix TEXT DEFAULT '',
			channel TEXT DEFAULT '',
			enabled INTEGER DEFAULT 1,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE card_blocklist (
			card_id INTEGER PRIMARY KEY,
			card_last_four TEXT DEFAULT '',
			reason TEXT NOT NULL DEFAULT '',
			distinct_emails INTEGER NOT NULL DEFAULT 0,
			fail_count INTEGER NOT NULL DEFAULT 0,
			freeze_status TEXT DEFAULT '',
			freeze_error TEXT DEFAULT '',
			blocked_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			unblocked_at DATETIME,
			notes TEXT DEFAULT ''
		);
		CREATE TABLE card_fail_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			card_id INTEGER NOT NULL,
			card_last_four TEXT DEFAULT '',
			order_id INTEGER NOT NULL DEFAULT 0,
			cdk_code TEXT DEFAULT '',
			account_email_norm TEXT NOT NULL DEFAULT '',
			email_source TEXT DEFAULT '',
			error_code TEXT DEFAULT '',
			order_status TEXT DEFAULT '',
			verdict TEXT DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(order_id, card_id)
		);
	`); err != nil {
		t.Fatal(err)
	}
	return database
}

func callPublicHandler(t *testing.T, router http.Handler, path string, body any) (int, map[string]any) {
	t.Helper()
	recorder, response := recordPublicHandler(t, router, path, body)
	return recorder.Code, response
}

func recordPublicHandler(t *testing.T, router http.Handler, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Redemption-Device", "public-security-test")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	response := map[string]any{}
	if recorder.Body.Len() > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode response (%d): %v body=%s", recorder.Code, err, recorder.Body.String())
		}
	}
	return recorder, response
}

func newPublicCardPlatformUpstream(t *testing.T, preview any, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/cdk/preview":
			_ = json.NewEncoder(w).Encode(preview)
		case "/api/v1/cdk/preflight":
			_ = json.NewEncoder(w).Encode(gin.H{
				"status":          "ready",
				"email":           "upstream.preflight@example.com",
				"preflight_token": "upstream-preflight-grant",
			})
		case "/api/v1/cdk/redeem":
			_ = json.NewEncoder(w).Encode(gin.H{"status": "submitted"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestPublicCDKPreviewFailsClosedWithoutDurableBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("successful upstream without token", func(t *testing.T) {
		database := openPublicHandlerTestDB(t, true)
		var calls atomic.Int32
		upstream := newPublicCardPlatformUpstream(t, gin.H{"status": "unused"}, &calls)
		t.Setenv("CARD_API_BASE", upstream.URL)

		router := gin.New()
		router.POST("/preview", PublicCDKPreview)
		status, response := callPublicHandler(t, router, "/preview", gin.H{"code": "PULS-NO-TOKEN"})
		if status != http.StatusBadGateway {
			t.Fatalf("missing-token preview status=%d response=%v", status, response)
		}
		if _, leaked := response["redemption_token"]; leaked {
			t.Fatalf("missing-token preview exposed a token: %v", response)
		}
		var count int
		if err := database.QueryRow(`SELECT COUNT(*) FROM cdk_session_bindings`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 || calls.Load() != 1 {
			t.Fatalf("missing-token preview binding=%d upstream_calls=%d", count, calls.Load())
		}
	})

	t.Run("binding persistence failure", func(t *testing.T) {
		database := openPublicHandlerTestDB(t, true)
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if err := database.Close(); err != nil {
				t.Errorf("close binding database: %v", err)
			}
			_ = json.NewEncoder(w).Encode(gin.H{
				"status":           "unused",
				"redemption_token": "upstream-token-must-not-escape",
			})
		}))
		defer upstream.Close()
		t.Setenv("CARD_API_BASE", upstream.URL)

		router := gin.New()
		router.POST("/preview", PublicCDKPreview)
		status, response := callPublicHandler(t, router, "/preview", gin.H{"code": "PULS-BIND-FAIL"})
		if status != http.StatusServiceUnavailable {
			t.Fatalf("binding-failure preview status=%d response=%v", status, response)
		}
		encoded, _ := json.Marshal(response)
		if bytes.Contains(encoded, []byte("upstream-token-must-not-escape")) {
			t.Fatalf("binding-failure preview leaked token: %s", encoded)
		}
		if calls.Load() != 1 {
			t.Fatalf("binding-failure upstream_calls=%d", calls.Load())
		}
	})

	for _, malformed := range []struct {
		name string
		code string
	}{
		{name: "missing code", code: ""},
		{name: "invalid code", code: "??"},
	} {
		t.Run(malformed.name+" cannot create an unbound token", func(t *testing.T) {
			database := openPublicHandlerTestDB(t, true)
			var calls atomic.Int32
			upstream := newPublicCardPlatformUpstream(t, gin.H{
				"status":           "unused",
				"redemption_token": "unbound-token-must-not-escape",
			}, &calls)
			t.Setenv("CARD_API_BASE", upstream.URL)

			router := gin.New()
			router.POST("/preview", PublicCDKPreview)
			status, response := callPublicHandler(t, router, "/preview", gin.H{"code": malformed.code})
			if status != http.StatusBadRequest {
				t.Fatalf("malformed-code preview status=%d response=%v", status, response)
			}
			encoded, _ := json.Marshal(response)
			if bytes.Contains(encoded, []byte("unbound-token-must-not-escape")) {
				t.Fatalf("malformed-code preview leaked token: %s", encoded)
			}
			var count int
			if err := database.QueryRow(`SELECT COUNT(*) FROM cdk_session_bindings`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 || calls.Load() != 0 {
				t.Fatalf("malformed-code preview bindings=%d upstream_calls=%d", count, calls.Load())
			}
		})
	}
}

func TestPublicCDKPreviewReturnsOpaqueAttemptToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openPublicHandlerTestDB(t, true)
	const (
		code  = "PULS-PREVIEW-ATTEMPT-TOKEN"
		token = "provider-preview-token"
	)
	var calls atomic.Int32
	upstream := newPublicCardPlatformUpstream(t, gin.H{
		"status":           "unused",
		"redemption_token": token,
	}, &calls)
	t.Setenv("CARD_API_BASE", upstream.URL)
	router := gin.New()
	router.POST("/preview", PublicCDKPreview)

	status, response := callPublicHandler(t, router, "/preview", gin.H{"code": code})
	if status != http.StatusOK {
		t.Fatalf("preview status=%d response=%v", status, response)
	}
	binding, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	attemptToken, _ := response["attempt_token"].(string)
	if attemptToken == "" || binding == nil || attemptToken != binding.AttemptNonce {
		t.Fatalf("preview attempt token=%q binding=%+v", attemptToken, binding)
	}
	if attemptToken == token {
		t.Fatal("local attempt token reused the upstream redemption token")
	}
	if binding.SubmitClaimedAt != 0 || binding.QueryStartedAt != 0 || binding.QueryExpiresAt != 0 {
		t.Fatalf("preview started submission window: %+v", binding)
	}
	if calls.Load() != 1 {
		t.Fatalf("preview upstream calls=%d", calls.Load())
	}
}

func TestPublicCDKPreviewRejectsActiveProcessingAttemptWithoutNewPreview(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openPublicHandlerTestDB(t, true)
	const (
		code  = "PULS-PREVIEW-ACTIVE"
		token = "preview-active-token"
	)
	if err := db.BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := db.ActivateCDKQueryWindowByToken(token, startedAt); err != nil {
		t.Fatal(err)
	}

	var resultCalls, previewCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/cdk/result":
			resultCalls.Add(1)
			_ = json.NewEncoder(w).Encode(gin.H{"status": "processing"})
		case "/api/v1/cdk/preview":
			previewCalls.Add(1)
			_ = json.NewEncoder(w).Encode(gin.H{
				"status":           "unused",
				"redemption_token": "must-not-replace-active-token",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	t.Setenv("CARD_API_BASE", upstream.URL)
	router := gin.New()
	router.POST("/preview", PublicCDKPreview)
	status, response := callPublicHandler(t, router, "/preview", gin.H{"code": code})
	if status != http.StatusConflict {
		t.Fatalf("active preview status=%d response=%v, want 409", status, response)
	}
	if resultCalls.Load() != 1 || previewCalls.Load() != 0 {
		t.Fatalf("active processing preview result_calls=%d preview_calls=%d",
			resultCalls.Load(), previewCalls.Load())
	}
	binding, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if binding.RedemptionToken != token || binding.QueryStartedAt != startedAt.Unix() ||
		binding.QueryExpiresAt == 0 {
		t.Fatalf("active preview changed binding: %+v", binding)
	}
}

func TestPublicCDKPreviewRetriesOnlyConfirmedFailedActiveAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database := openPublicHandlerTestDB(t, true)
	const (
		code  = "PULS-PREVIEW-FAILED-RETRY"
		token = "provider-reused-retry-token"
	)
	if err := db.BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	before, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BindCDKAccountEmailForAttempt(code, token, before.AttemptNonce, "old.attempt@example.com"); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := db.ActivateCDKQueryWindowByToken(token, startedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
		INSERT INTO recharge_tasks(task_id, cdk_code, session_json)
		VALUES ('failed-retry-old-task', ?, 'old-attempt-session')
	`, code); err != nil {
		t.Fatal(err)
	}
	active, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}

	var resultCalls, previewCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/cdk/result":
			resultCalls.Add(1)
			if got := r.URL.Query().Get("token"); got != token {
				t.Errorf("result token=%q, want %q", got, token)
			}
			_ = json.NewEncoder(w).Encode(gin.H{
				"status":       "success",
				"can_resubmit": false,
				"data": gin.H{"order": gin.H{
					"status":       "failed",
					"can_resubmit": true,
				}},
			})
		case "/api/v1/cdk/preview":
			previewCalls.Add(1)
			_ = json.NewEncoder(w).Encode(gin.H{
				"status":           "unused",
				"redemption_token": token,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	t.Setenv("CARD_API_BASE", upstream.URL)
	router := gin.New()
	router.POST("/preview", PublicCDKPreview)
	status, response := callPublicHandler(t, router, "/preview", gin.H{"code": code})
	if status != http.StatusOK {
		t.Fatalf("failed-attempt retry status=%d response=%v", status, response)
	}
	if resultCalls.Load() != 1 || previewCalls.Load() != 1 {
		t.Fatalf("failed-attempt retry result_calls=%d preview_calls=%d",
			resultCalls.Load(), previewCalls.Load())
	}
	fresh, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.RedemptionToken != token || fresh.AttemptNonce == "" ||
		fresh.AttemptNonce == active.AttemptNonce || fresh.AccountEmail != "" ||
		fresh.SessionPayload != "" || fresh.SubmitClaimedAt != 0 ||
		fresh.QueryStartedAt != 0 || fresh.QueryExpiresAt != 0 {
		t.Fatalf("failed retry did not create a clean pending attempt: %+v", fresh)
	}
	if got, _ := response["attempt_token"].(string); got == "" || got != fresh.AttemptNonce {
		t.Fatalf("failed retry did not return the new local attempt token: response=%v binding=%+v", response, fresh)
	}
	var taskSession sql.NullString
	if err := database.QueryRow(`
		SELECT session_json FROM recharge_tasks WHERE task_id='failed-retry-old-task'
	`).Scan(&taskSession); err != nil {
		t.Fatal(err)
	}
	if taskSession.Valid {
		t.Fatalf("failed retry retained old task session: %q", taskSession.String)
	}
	const retryPreflight = "failed-retry-new-preflight"
	retrySubmittedAt := time.Now().UTC().Truncate(time.Second)
	if err := db.RegisterCDKPreflightGrant(code, token, fresh.AttemptNonce, retryPreflight, retrySubmittedAt); err != nil {
		t.Fatal(err)
	}
	if err := db.ClaimCDKRedemptionAttempt(token, fresh.AttemptNonce, retryPreflight, retrySubmittedAt); err != nil {
		t.Fatal(err)
	}
	retried, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if retried.QueryStartedAt != retrySubmittedAt.Unix() ||
		retried.QueryExpiresAt != retrySubmittedAt.Add(publicCDKQueryWindow).Unix() ||
		retried.QueryStartedAt <= active.QueryStartedAt {
		t.Fatalf("confirmed failed attempt did not start a distinct new seven-day window: old=%+v new=%+v", active, retried)
	}
}

func TestPublicCDKPreviewRejectsFailedAttemptWithoutExplicitRetryPermission(t *testing.T) {
	for _, test := range []struct {
		name              string
		includePermission bool
		canResubmit       bool
	}{
		{name: "missing can_resubmit"},
		{name: "can_resubmit false", includePermission: true, canResubmit: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			openPublicHandlerTestDB(t, true)
			code := "PULS-FAILED-NO-RETRY-" + strings.ToUpper(strings.ReplaceAll(test.name, " ", "-"))
			token := "failed-no-retry-token-" + test.name
			if err := db.BindCDKRedemptionToken(code, token); err != nil {
				t.Fatal(err)
			}
			if err := db.ActivateCDKQueryWindowByToken(token, time.Now().Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			before, err := db.GetBindingByCDK(code)
			if err != nil {
				t.Fatal(err)
			}
			var resultCalls, previewCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/cdk/result":
					resultCalls.Add(1)
					order := gin.H{"status": "failed"}
					if test.includePermission {
						order["can_resubmit"] = test.canResubmit
					}
					_ = json.NewEncoder(w).Encode(gin.H{
						"status": "success",
						"data":   gin.H{"order": order},
					})
				case "/api/v1/cdk/preview":
					previewCalls.Add(1)
					_ = json.NewEncoder(w).Encode(gin.H{
						"status":           "unused",
						"redemption_token": "must-not-replace-locked-attempt",
					})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer upstream.Close()
			t.Setenv("CARD_API_BASE", upstream.URL)
			router := gin.New()
			router.POST("/preview", PublicCDKPreview)
			status, response := callPublicHandler(t, router, "/preview", gin.H{"code": code})
			if status != http.StatusConflict {
				t.Fatalf("locked failed attempt status=%d response=%v", status, response)
			}
			if resultCalls.Load() != 1 || previewCalls.Load() != 0 {
				t.Fatalf("locked failed attempt calls=result:%d preview:%d", resultCalls.Load(), previewCalls.Load())
			}
			after, err := db.GetBindingByCDK(code)
			if err != nil {
				t.Fatal(err)
			}
			if after.RedemptionToken != before.RedemptionToken || after.AttemptNonce != before.AttemptNonce ||
				after.QueryStartedAt != before.QueryStartedAt || after.QueryExpiresAt != before.QueryExpiresAt {
				t.Fatalf("locked failed attempt changed binding: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestPublicCDKPreviewDoesNotRetryNonFailedActiveStatus(t *testing.T) {
	for _, upstreamStatus := range []string{"processing", "completed", "mystery_provider_state"} {
		t.Run(upstreamStatus, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			openPublicHandlerTestDB(t, true)
			code := "PULS-NO-RETRY-" + strings.ToUpper(upstreamStatus)
			token := "no-retry-token-" + upstreamStatus
			if err := db.BindCDKRedemptionToken(code, token); err != nil {
				t.Fatal(err)
			}
			if err := db.ActivateCDKQueryWindowByToken(token, time.Now().Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			before, err := db.GetBindingByCDK(code)
			if err != nil {
				t.Fatal(err)
			}

			var resultCalls, previewCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/cdk/result":
					resultCalls.Add(1)
					_ = json.NewEncoder(w).Encode(gin.H{"status": upstreamStatus})
				case "/api/v1/cdk/preview":
					previewCalls.Add(1)
					_ = json.NewEncoder(w).Encode(gin.H{
						"status":           "unused",
						"redemption_token": "must-not-be-saved",
					})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer upstream.Close()
			t.Setenv("CARD_API_BASE", upstream.URL)
			router := gin.New()
			router.POST("/preview", PublicCDKPreview)
			status, response := callPublicHandler(t, router, "/preview", gin.H{"code": code})
			if status != http.StatusConflict {
				t.Fatalf("nonfailed active status=%q response=%d/%v", upstreamStatus, status, response)
			}
			if resultCalls.Load() != 1 || previewCalls.Load() != 0 {
				t.Fatalf("nonfailed status=%q result_calls=%d preview_calls=%d",
					upstreamStatus, resultCalls.Load(), previewCalls.Load())
			}
			after, err := db.GetBindingByCDK(code)
			if err != nil {
				t.Fatal(err)
			}
			if after.RedemptionToken != before.RedemptionToken ||
				after.AttemptNonce != before.AttemptNonce ||
				after.QueryStartedAt != before.QueryStartedAt ||
				after.QueryExpiresAt != before.QueryExpiresAt {
				t.Fatalf("nonfailed preview changed active binding: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestPublicCDKRedeemNeverCallsUpstreamWithoutActivatedBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, test := range []struct {
		name         string
		token        string
		attemptToken string
		preflight    string
		closeDB      bool
		wantStatus   int
	}{
		{name: "missing token", token: "", attemptToken: "some-attempt", preflight: "some-preflight", wantStatus: http.StatusBadRequest},
		{name: "missing attempt handle", token: "known-looking-token", preflight: "some-preflight", wantStatus: http.StatusBadRequest},
		{name: "missing preflight grant", token: "known-looking-token", attemptToken: "known-looking-attempt", wantStatus: http.StatusBadRequest},
		{name: "unknown token", token: "unknown-redemption-token", attemptToken: "unknown-attempt", preflight: "unknown-preflight", wantStatus: http.StatusConflict},
		{name: "activation database failure", token: "known-looking-token", attemptToken: "known-looking-attempt", preflight: "known-looking-preflight", closeDB: true, wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := openPublicHandlerTestDB(t, true)
			if test.closeDB {
				if err := database.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int32
			upstream := newPublicCardPlatformUpstream(t, gin.H{}, &calls)
			t.Setenv("CARD_API_BASE", upstream.URL)

			router := gin.New()
			router.POST("/redeem", PublicCDKRedeem)
			status, response := callPublicHandler(t, router, "/redeem", gin.H{
				"redemption_token": test.token,
				"attempt_token":    test.attemptToken,
				"preflight_token":  test.preflight,
				"confirmed":        true,
			})
			if status != test.wantStatus {
				t.Fatalf("redeem status=%d response=%v, want %d", status, response, test.wantStatus)
			}
			if calls.Load() != 0 {
				t.Fatalf("redeem called upstream %d times without activated binding", calls.Load())
			}
		})
	}
}

func TestPublicCDKRedeemPolicySnapshotFailsBeforeClaimAndUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name   string
		breaks func(t *testing.T, database *sql.DB)
	}{
		{
			name: "malformed site policy",
			breaks: func(t *testing.T, database *sql.DB) {
				t.Helper()
				if _, err := database.Exec(`
					INSERT INTO site_settings(key, value) VALUES ('site_redeem_policy', '{not-json')
				`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "blocklist read failure",
			breaks: func(t *testing.T, database *sql.DB) {
				t.Helper()
				if _, err := database.Exec(`DROP TABLE card_blocklist`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "selection rule read failure",
			breaks: func(t *testing.T, database *sql.DB) {
				t.Helper()
				if _, err := database.Exec(`DROP TABLE card_selection_rules`); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := openPublicHandlerTestDB(t, true)
			code := "PULS-POLICY-FAIL-" + strings.ToUpper(strings.ReplaceAll(test.name, " ", "-"))
			token := "policy-fail-token-" + test.name
			preflight := "policy-fail-preflight-" + test.name
			if err := db.BindCDKRedemptionToken(code, token); err != nil {
				t.Fatal(err)
			}
			pending, err := db.GetBindingByCDK(code)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.RegisterCDKPreflightGrant(code, token, pending.AttemptNonce, preflight, time.Now()); err != nil {
				t.Fatal(err)
			}
			test.breaks(t, database)

			var calls atomic.Int32
			upstream := newPublicCardPlatformUpstream(t, gin.H{}, &calls)
			t.Setenv("CARD_API_BASE", upstream.URL)
			router := gin.New()
			router.POST("/redeem", PublicCDKRedeem)
			status, response := callPublicHandler(t, router, "/redeem", gin.H{
				"redemption_token": token,
				"attempt_token":    pending.AttemptNonce,
				"preflight_token":  preflight,
			})
			if status != http.StatusServiceUnavailable {
				t.Fatalf("unsafe policy snapshot status=%d response=%v", status, response)
			}
			if calls.Load() != 0 {
				t.Fatalf("unsafe policy snapshot reached upstream %d times", calls.Load())
			}
			current, err := db.GetBindingByCDK(code)
			if err != nil {
				t.Fatal(err)
			}
			if current.SubmitClaimedAt != 0 || current.QueryStartedAt != 0 || current.QueryExpiresAt != 0 {
				t.Fatalf("unsafe policy snapshot consumed the submit latch: %+v", current)
			}
		})
	}
}

func TestPublicCDKRedeemSubmitStateHeaderTracksClaimBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openPublicHandlerTestDB(t, true)
	const (
		code      = "PULS-SUBMIT-STATE-HEADER"
		token     = "submit-state-redemption-token"
		preflight = "submit-state-preflight-token"
	)
	if err := db.BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	pending, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterCDKPreflightGrant(code, token, pending.AttemptNonce, preflight, time.Now()); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := newPublicCardPlatformUpstream(t, gin.H{}, &calls)
	t.Setenv("CARD_API_BASE", upstream.URL)
	router := gin.New()
	router.POST("/redeem", PublicCDKRedeem)

	beforeClaim, response := recordPublicHandler(t, router, "/redeem", gin.H{
		"redemption_token": token,
		"attempt_token":    pending.AttemptNonce,
		"preflight_token":  "not-a-registered-grant",
	})
	if beforeClaim.Code != http.StatusConflict || beforeClaim.Header().Get("X-Maple-Submit-State") != "not-submitted" {
		t.Fatalf("pre-claim response=%d/%v submit-state=%q", beforeClaim.Code, response,
			beforeClaim.Header().Get("X-Maple-Submit-State"))
	}
	if calls.Load() != 0 {
		t.Fatalf("pre-claim failure reached upstream %d times", calls.Load())
	}

	body := gin.H{
		"redemption_token": token,
		"attempt_token":    pending.AttemptNonce,
		"preflight_token":  preflight,
	}
	submitted, response := recordPublicHandler(t, router, "/redeem", body)
	if submitted.Code != http.StatusOK || submitted.Header().Get("X-Maple-Submit-State") != "submitted" {
		t.Fatalf("claimed response=%d/%v submit-state=%q", submitted.Code, response,
			submitted.Header().Get("X-Maple-Submit-State"))
	}
	replay, response := recordPublicHandler(t, router, "/redeem", body)
	if replay.Code != http.StatusConflict || replay.Header().Get("X-Maple-Submit-State") != "submitted" {
		t.Fatalf("replayed response=%d/%v submit-state=%q", replay.Code, response,
			replay.Header().Get("X-Maple-Submit-State"))
	}
	if calls.Load() != 1 {
		t.Fatalf("claimed/replayed upstream calls=%d, want 1", calls.Load())
	}
}

func TestPublicCDKConcurrentRedeemCallsUpstreamOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openPublicHandlerTestDB(t, true)
	const (
		code      = "PULS-CONCURRENT-REDEEM"
		token     = "concurrent-redeem-token"
		preflight = "concurrent-redeem-preflight"
	)
	if err := db.BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	pending, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterCDKPreflightGrant(code, token, pending.AttemptNonce, preflight, time.Now()); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		_ = json.NewEncoder(w).Encode(gin.H{
			"status":        "submitted",
			"account_email": "redeemed.customer@example.com",
		})
	}))
	defer upstream.Close()
	t.Setenv("CARD_API_BASE", upstream.URL)
	router := gin.New()
	router.POST("/redeem", PublicCDKRedeem)
	body := gin.H{
		"redemption_token": token,
		"attempt_token":    pending.AttemptNonce,
		"preflight_token":  preflight,
		"confirmed":        true,
	}
	firstResponse := asyncPublicHandlerCall(router, "/redeem", body)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("first redeem did not reach upstream")
	}
	secondResponse := asyncPublicHandlerCall(router, "/redeem", body)
	var second publicHandlerResponse
	select {
	case second = <-secondResponse:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("replayed redeem did not fail before upstream")
	}
	if second.status != http.StatusConflict {
		close(release)
		t.Fatalf("replayed redeem status=%d response=%v", second.status, second.body)
	}
	close(release)
	first := <-firstResponse
	if first.status != http.StatusOK {
		t.Fatalf("first redeem status=%d response=%v", first.status, first.body)
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent redeem upstream calls=%d, want 1", calls.Load())
	}
	binding, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if binding.SubmitClaimedAt == 0 || binding.QueryStartedAt == 0 || binding.QueryExpiresAt == 0 ||
		binding.AccountEmail != "redeemed.customer@example.com" {
		t.Fatalf("first redeem did not leave one authoritative active attempt: %+v", binding)
	}
}

func TestPublicCDKRedeemNetworkAmbiguityKeepsSubmitLatchClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openPublicHandlerTestDB(t, true)
	const (
		code      = "PULS-REDEEM-NETWORK-AMBIGUITY"
		token     = "redeem-network-ambiguity-token"
		preflight = "redeem-network-ambiguity-preflight"
	)
	if err := db.BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	pending, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterCDKPreflightGrant(code, token, pending.AttemptNonce, preflight, time.Now()); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test upstream does not support hijacking")
			return
		}
		connection, _, hijackErr := hijacker.Hijack()
		if hijackErr != nil {
			t.Errorf("hijack upstream connection: %v", hijackErr)
			return
		}
		_ = connection.Close()
	}))
	defer upstream.Close()
	t.Setenv("CARD_API_BASE", upstream.URL)
	router := gin.New()
	router.POST("/redeem", PublicCDKRedeem)
	body := gin.H{
		"redemption_token": token,
		"attempt_token":    pending.AttemptNonce,
		"preflight_token":  preflight,
		"confirmed":        true,
	}
	firstStatus, firstResponse := callPublicHandler(t, router, "/redeem", body)
	if firstStatus != http.StatusBadGateway {
		t.Fatalf("ambiguous redeem status=%d response=%v", firstStatus, firstResponse)
	}
	secondStatus, secondResponse := callPublicHandler(t, router, "/redeem", body)
	if secondStatus != http.StatusConflict {
		t.Fatalf("ambiguous redeem retry status=%d response=%v", secondStatus, secondResponse)
	}
	if calls.Load() != 1 {
		t.Fatalf("ambiguous redeem retry reached upstream; calls=%d", calls.Load())
	}
	binding, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if binding.SubmitClaimedAt == 0 || binding.QueryStartedAt == 0 || binding.QueryExpiresAt == 0 {
		t.Fatalf("ambiguous first submission did not preserve the closed latch: %+v", binding)
	}
}

func TestPublicCDKRedeemRejectsUnregisteredPreflightGrantBeforeUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openPublicHandlerTestDB(t, true)
	const (
		code       = "PULS-REDEEM-PREFLIGHT-GRANT"
		token      = "redeem-preflight-grant-token"
		validGrant = "registered-preflight-grant"
	)
	if err := db.BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	pending, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterCDKPreflightGrant(code, token, pending.AttemptNonce, validGrant, time.Now()); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := newPublicCardPlatformUpstream(t, gin.H{}, &calls)
	t.Setenv("CARD_API_BASE", upstream.URL)
	router := gin.New()
	router.POST("/redeem", PublicCDKRedeem)

	status, response := callPublicHandler(t, router, "/redeem", gin.H{
		"redemption_token": token,
		"attempt_token":    pending.AttemptNonce,
		"preflight_token":  "unregistered-preflight-grant",
		"confirmed":        true,
	})
	if status != http.StatusConflict {
		t.Fatalf("unregistered preflight grant status=%d response=%v", status, response)
	}
	if calls.Load() != 0 {
		t.Fatalf("unregistered preflight grant reached upstream %d times", calls.Load())
	}
	stillPending, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if stillPending.SubmitClaimedAt != 0 || stillPending.QueryStartedAt != 0 || stillPending.QueryExpiresAt != 0 {
		t.Fatalf("unregistered preflight grant consumed the attempt: %+v", stillPending)
	}

	status, response = callPublicHandler(t, router, "/redeem", gin.H{
		"redemption_token": token,
		"attempt_token":    pending.AttemptNonce,
		"preflight_token":  validGrant,
		"confirmed":        true,
	})
	if status != http.StatusOK {
		t.Fatalf("registered preflight grant status=%d response=%v", status, response)
	}
	if calls.Load() != 1 {
		t.Fatalf("registered preflight grant upstream calls=%d, want 1", calls.Load())
	}
}

func TestPublicCDKRedeemNormalizesTokensAndRebuildsAllowlistedUpstreamBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openPublicHandlerTestDB(t, true)
	const (
		code       = "PULS-REDEEM-ALLOWLIST"
		token      = "allowlist-redemption-token"
		preflight  = "allowlist-preflight-token"
		credential = "must-not-reach-upstream"
	)
	if err := db.BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	pending, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterCDKPreflightGrant(code, token, pending.AttemptNonce, preflight, time.Now()); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	var captured map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode upstream redeem body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gin.H{"status": "submitted"})
	}))
	defer upstream.Close()
	t.Setenv("CARD_API_BASE", upstream.URL)
	router := gin.New()
	router.POST("/redeem", PublicCDKRedeem)

	request := gin.H{
		"redemption_token":       "  " + token + "\n",
		"attempt_token":          "\t" + pending.AttemptNonce + " ",
		"preflight_token":        " " + preflight + "\r\n",
		"client_request_id":      "attacker-controlled-id",
		"strict":                 "ATTACKER_STRICT",
		"strict_card_preference": "ATTACKER_STRICT_CARD_PREF",
		"no_auto_card_switch":    "ATTACKER_NO_AUTO",
		"exclude_card_ids":       []any{"ATTACKER_CARD"},
		"card_id":                998877,
		"credential":             credential,
		"session":                "attacker-session",
		"code":                   "PULS-OTHER-CODE",
		"confirmed":              true,
	}
	status, response := callPublicHandler(t, router, "/redeem", request)
	if status != http.StatusOK {
		t.Fatalf("allowlisted redeem status=%d response=%v", status, response)
	}
	if calls.Load() != 1 {
		t.Fatalf("allowlisted redeem upstream calls=%d, want 1", calls.Load())
	}
	if captured["redemption_token"] != token || captured["preflight_token"] != preflight {
		t.Fatalf("upstream tokens were not normalized: %v", captured)
	}
	wantHash := sha256.Sum256([]byte(pending.AttemptNonce))
	wantRequestID := "maple-cdk-" + hex.EncodeToString(wantHash[:])[:24]
	if captured["client_request_id"] != wantRequestID || !strings.HasPrefix(wantRequestID, "maple-cdk-") {
		t.Fatalf("server request id=%v, want stable %q", captured["client_request_id"], wantRequestID)
	}
	for _, key := range []string{
		"strict", "strict_card_preference", "no_auto_card_switch", "exclude_card_ids",
		"card_id", "credential", "session", "code", "confirmed", "attempt_token",
	} {
		if _, exists := captured[key]; exists {
			t.Fatalf("caller-controlled field %q crossed public redeem boundary: %v", key, captured)
		}
	}
	encoded, _ := json.Marshal(captured)
	for _, secret := range []string{
		"attacker-controlled-id", "ATTACKER_STRICT", "ATTACKER_STRICT_CARD_PREF",
		"ATTACKER_NO_AUTO", "ATTACKER_CARD", credential, "attacker-session", "PULS-OTHER-CODE",
	} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("caller value %q reached upstream: %s", secret, encoded)
		}
	}

	// Whitespace-normalized replays resolve to the same immutable attempt and
	// are rejected by the one-way claim before a second upstream request.
	replayStatus, replayResponse := callPublicHandler(t, router, "/redeem", request)
	if replayStatus != http.StatusConflict {
		t.Fatalf("normalized replay status=%d response=%v", replayStatus, replayResponse)
	}
	if calls.Load() != 1 {
		t.Fatalf("normalized replay reached upstream; calls=%d", calls.Load())
	}
}

func TestPublicCDKPreflightRejectsStaleBindingBeforeUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database := openPublicHandlerTestDB(t, true)
	const (
		code  = "PULS-STALE-PREFLIGHT"
		token = "stale-preflight-token"
	)
	if err := db.BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	binding, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	staleAt := time.Now().UTC().Add(-24 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := database.Exec(`
		UPDATE cdk_session_bindings SET updated_at=? WHERE cdk_code=?
	`, staleAt, code); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	upstream := newPublicCardPlatformUpstream(t, gin.H{}, &calls)
	t.Setenv("CARD_API_BASE", upstream.URL)
	router := gin.New()
	router.POST("/preflight", PublicCDKPreflight)

	status, response := callPublicHandler(t, router, "/preflight", gin.H{
		"code":             code,
		"redemption_token": token,
		"attempt_token":    binding.AttemptNonce,
		"credential":       gin.H{"email": "attacker@example.com"},
	})
	if status != http.StatusGone {
		t.Fatalf("stale preflight status=%d response=%v, want 410", status, response)
	}
	if calls.Load() != 0 {
		t.Fatalf("stale preflight called upstream %d times", calls.Load())
	}
	encoded, _ := json.Marshal(response)
	if bytes.Contains(encoded, []byte("attacker@example.com")) ||
		bytes.Contains(encoded, []byte("upstream.preflight@example.com")) {
		t.Fatalf("stale preflight leaked identity: %s", encoded)
	}
}

func TestPublicCDKPreflightRejectsCodeTokenMismatchWithoutMutation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database := openPublicHandlerTestDB(t, true)
	const (
		boundCode = "PULS-PREFLIGHT-BOUND-CODE"
		bodyCode  = "PULS-PREFLIGHT-OTHER-CODE"
		token     = "preflight-bound-token"
	)
	if err := db.BindCDKRedemptionToken(boundCode, token); err != nil {
		t.Fatal(err)
	}
	binding, err := db.GetBindingByCDK(boundCode)
	if err != nil {
		t.Fatal(err)
	}
	var beforeToken, beforeEmail, beforeUpdated string
	if err := database.QueryRow(`
		SELECT redemption_token, account_email, updated_at
		FROM cdk_session_bindings WHERE cdk_code=?
	`, boundCode).Scan(&beforeToken, &beforeEmail, &beforeUpdated); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	upstream := newPublicCardPlatformUpstream(t, gin.H{}, &calls)
	t.Setenv("CARD_API_BASE", upstream.URL)
	router := gin.New()
	router.POST("/preflight", PublicCDKPreflight)

	status, response := callPublicHandler(t, router, "/preflight", gin.H{
		"code":             bodyCode,
		"redemption_token": token,
		"attempt_token":    binding.AttemptNonce,
		"credential":       gin.H{"email": "attacker@example.com"},
	})
	if status != http.StatusConflict {
		t.Fatalf("mismatched preflight status=%d response=%v, want 409", status, response)
	}
	if calls.Load() != 0 {
		t.Fatalf("mismatched preflight called upstream %d times", calls.Load())
	}
	var afterToken, afterEmail, afterUpdated string
	if err := database.QueryRow(`
		SELECT redemption_token, account_email, updated_at
		FROM cdk_session_bindings WHERE cdk_code=?
	`, boundCode).Scan(&afterToken, &afterEmail, &afterUpdated); err != nil {
		t.Fatal(err)
	}
	if afterToken != beforeToken || afterEmail != beforeEmail || afterUpdated != beforeUpdated {
		t.Fatalf("mismatched preflight mutated bound row: before=%q/%q/%q after=%q/%q/%q",
			beforeToken, beforeEmail, beforeUpdated, afterToken, afterEmail, afterUpdated)
	}
	var otherCount int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM cdk_session_bindings WHERE cdk_code=?
	`, bodyCode).Scan(&otherCount); err != nil {
		t.Fatal(err)
	}
	if otherCount != 0 {
		t.Fatalf("mismatched preflight created %d rows for body code", otherCount)
	}
}

func TestPublicCDKOldAttemptHandleCannotUseReusedProviderToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openPublicHandlerTestDB(t, true)
	const (
		code  = "PULS-REUSED-PROVIDER-TOKEN"
		token = "provider-reused-token"
	)
	if err := db.BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	first, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	const oldPreflightGrant = "old-preflight-grant"
	if err := db.RegisterCDKPreflightGrant(code, token, first.AttemptNonce, oldPreflightGrant, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.BindCDKRedemptionTokenCAS(code, token, first); err != nil {
		t.Fatal(err)
	}
	second, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if first.AttemptNonce == "" || second.AttemptNonce == "" || first.AttemptNonce == second.AttemptNonce {
		t.Fatalf("same provider token did not rotate local attempt handle: first=%+v second=%+v", first, second)
	}
	var oldGrantCount int
	if err := db.DB.QueryRow(`
		SELECT COUNT(*) FROM cdk_preflight_grants WHERE attempt_nonce=?
	`, first.AttemptNonce).Scan(&oldGrantCount); err != nil {
		t.Fatal(err)
	}
	if oldGrantCount != 0 {
		t.Fatalf("new preview retained %d grants from old local attempt", oldGrantCount)
	}

	var calls atomic.Int32
	upstream := newPublicCardPlatformUpstream(t, gin.H{}, &calls)
	t.Setenv("CARD_API_BASE", upstream.URL)
	router := gin.New()
	router.POST("/preflight", PublicCDKPreflight)
	router.POST("/redeem", PublicCDKRedeem)

	for _, request := range []struct {
		name string
		path string
		body gin.H
	}{
		{
			name: "preflight",
			path: "/preflight",
			body: gin.H{
				"code":             code,
				"redemption_token": token,
				"attempt_token":    first.AttemptNonce,
				"credential":       gin.H{"email": "old-handle@example.com"},
			},
		},
		{
			name: "redeem",
			path: "/redeem",
			body: gin.H{
				"redemption_token": token,
				"attempt_token":    first.AttemptNonce,
				"preflight_token":  oldPreflightGrant,
				"confirmed":        true,
			},
		},
	} {
		t.Run(request.name, func(t *testing.T) {
			status, response := callPublicHandler(t, router, request.path, request.body)
			if status != http.StatusConflict {
				t.Fatalf("old-handle %s status=%d response=%v", request.name, status, response)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("old attempt handle reached upstream %d times", calls.Load())
	}
	after, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if after.AttemptNonce != second.AttemptNonce || after.RedemptionToken != token ||
		after.SubmitClaimedAt != 0 || after.QueryStartedAt != 0 || after.QueryExpiresAt != 0 {
		t.Fatalf("old attempt handle mutated the current attempt: before=%+v after=%+v", second, after)
	}
}

func TestPublicCDKPreflightRejectsMissingAttemptHandleBeforeUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openPublicHandlerTestDB(t, true)
	const (
		code  = "PULS-PREFLIGHT-MISSING-HANDLE"
		token = "preflight-missing-handle-token"
	)
	if err := db.BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := newPublicCardPlatformUpstream(t, gin.H{}, &calls)
	t.Setenv("CARD_API_BASE", upstream.URL)
	router := gin.New()
	router.POST("/preflight", PublicCDKPreflight)
	status, response := callPublicHandler(t, router, "/preflight", gin.H{
		"code":             code,
		"redemption_token": token,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("missing attempt handle status=%d response=%v", status, response)
	}
	if calls.Load() != 0 {
		t.Fatalf("missing attempt handle reached upstream %d times", calls.Load())
	}
}

func TestPublicCDKPreflightFailsClosedWithoutGrantToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openPublicHandlerTestDB(t, true)
	const (
		code  = "PULS-PREFLIGHT-NO-GRANT"
		token = "preflight-no-grant-redemption"
	)
	if err := db.BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	pending, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(gin.H{
			"status": "ready",
			"email":  "must.not.persist@example.com",
		})
	}))
	defer upstream.Close()
	t.Setenv("CARD_API_BASE", upstream.URL)
	router := gin.New()
	router.POST("/preflight", PublicCDKPreflight)
	status, response := callPublicHandler(t, router, "/preflight", gin.H{
		"code":             code,
		"redemption_token": token,
		"attempt_token":    pending.AttemptNonce,
		"credential":       gin.H{"email": "must.not.persist@example.com"},
	})
	if status != http.StatusBadGateway {
		t.Fatalf("missing preflight grant status=%d response=%v", status, response)
	}
	if calls.Load() != 1 {
		t.Fatalf("missing preflight grant upstream calls=%d", calls.Load())
	}
	var grantCount int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM cdk_preflight_grants`).Scan(&grantCount); err != nil {
		t.Fatal(err)
	}
	current, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if grantCount != 0 || current.AccountEmail != "" || current.SubmitClaimedAt != 0 ||
		current.QueryStartedAt != 0 || current.QueryExpiresAt != 0 {
		t.Fatalf("missing grant response mutated pending attempt: grants=%d binding=%+v", grantCount, current)
	}
}

func TestPublicCDKConcurrentPreflightNeverPersistsEmail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openPublicHandlerTestDB(t, true)
	const (
		code  = "PULS-PREFLIGHT-CONCURRENT"
		token = "preflight-concurrent-token"
	)
	if err := db.BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	pending, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if pending.AttemptNonce == "" {
		t.Fatal("preview did not create an attempt token")
	}

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		_ = json.NewEncoder(w).Encode(gin.H{
			"status":          "ready",
			"email":           fmt.Sprintf("preflight-%d.private@example.com", call),
			"preflight_token": fmt.Sprintf("concurrent-preflight-grant-%d", call),
		})
	}))
	defer upstream.Close()
	t.Setenv("CARD_API_BASE", upstream.URL)
	router := gin.New()
	router.POST("/preflight", PublicCDKPreflight)
	body := gin.H{
		"code":             code,
		"redemption_token": token,
		"attempt_token":    pending.AttemptNonce,
		"credential":       gin.H{"email": "credential.private@example.com"},
	}
	firstResponse := asyncPublicHandlerCall(router, "/preflight", body)
	secondResponse := asyncPublicHandlerCall(router, "/preflight", body)
	for index, response := range []publicHandlerResponse{<-firstResponse, <-secondResponse} {
		if response.status != http.StatusOK {
			t.Fatalf("concurrent preflight %d status=%d response=%v", index+1, response.status, response.body)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("concurrent preflight upstream calls=%d, want 2", calls.Load())
	}
	var grantCount int
	var storedHashes string
	if err := db.DB.QueryRow(`
		SELECT COUNT(*), COALESCE(GROUP_CONCAT(token_hash, ','), '')
		FROM cdk_preflight_grants WHERE attempt_nonce=?
	`, pending.AttemptNonce).Scan(&grantCount, &storedHashes); err != nil {
		t.Fatal(err)
	}
	if grantCount != 2 || strings.Contains(storedHashes, "concurrent-preflight-grant-") {
		t.Fatalf("preflight grants were not stored as two opaque hashes: count=%d hashes=%q", grantCount, storedHashes)
	}
	current, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if current.AttemptNonce != pending.AttemptNonce || current.AccountEmail != "" ||
		current.SubmitClaimedAt != 0 || current.QueryStartedAt != 0 || current.QueryExpiresAt != 0 ||
		current.UpdatedAt != pending.UpdatedAt {
		t.Fatalf("concurrent preflight persisted identity or changed the attempt: before=%+v after=%+v", pending, current)
	}
}

func TestPublicCDKResultAttemptNonceRejectsSameTokenABAEmail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database := openPublicHandlerTestDB(t, true)
	const (
		code  = "PULS-RESULT-NONCE-ABA"
		token = "result-provider-reused-token"
	)
	if err := db.BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateCDKQueryWindowByToken(token, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	oldAttempt, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(gin.H{
			"status": "success",
			"data": gin.H{
				"order": gin.H{
					"id":            771122,
					"card_id":       889944,
					"status":        "failed",
					"account_email": "old.result.private@example.com",
				},
			},
		})
	}))
	defer upstream.Close()
	t.Setenv("CARD_API_BASE", upstream.URL)
	router := gin.New()
	router.POST("/result", PublicCDKResult)
	responseCh := asyncPublicHandlerCall(router, "/result", gin.H{
		"token":         token,
		"attempt_token": oldAttempt.AttemptNonce,
	})
	<-entered
	pastStart := time.Now().UTC().Add(-8 * 24 * time.Hour).Unix()
	if _, err := database.Exec(`
		UPDATE cdk_session_bindings
		SET query_started_at=?, query_expires_at=?
		WHERE cdk_code=?
	`, pastStart, pastStart+int64(7*24*time.Hour/time.Second), code); err != nil {
		close(release)
		t.Fatal(err)
	}
	expiredSnapshot, err := db.GetBindingByCDK(code)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	if err := db.BindCDKRedemptionTokenCAS(code, token, expiredSnapshot); err != nil {
		close(release)
		t.Fatal(err)
	}
	newAttempt, err := db.GetBindingByCDK(code)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	if newAttempt.AttemptNonce == oldAttempt.AttemptNonce {
		close(release)
		t.Fatal("expired same-token preview did not rotate attempt nonce")
	}
	close(release)
	response := <-responseCh
	if response.status != http.StatusNotFound {
		t.Fatalf("in-flight stale result status=%d response=%v, want 404", response.status, response.body)
	}
	encodedResponse, _ := json.Marshal(response.body)
	for _, secret := range []string{"old.result.private@example.com", "771122", "889944"} {
		if bytes.Contains(encodedResponse, []byte(secret)) {
			t.Fatalf("in-flight stale result leaked %q: %s", secret, encodedResponse)
		}
	}
	current, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if current.AttemptNonce != newAttempt.AttemptNonce || current.AccountEmail != "" ||
		current.QueryStartedAt != 0 || current.QueryExpiresAt != 0 {
		t.Fatalf("old result response wrote into new attempt: %+v", current)
	}
	// The result observer runs asynchronously. Give the stale observer time to
	// complete its current-attempt guard, then prove it did not teach the card
	// health subsystem from the nested failed order.
	time.Sleep(250 * time.Millisecond)
	var failEvents, blockedCards int
	if err := database.QueryRow(`SELECT COUNT(*) FROM card_fail_events`).Scan(&failEvents); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM card_blocklist`).Scan(&blockedCards); err != nil {
		t.Fatal(err)
	}
	if failEvents != 0 || blockedCards != 0 {
		t.Fatalf("stale result observer wrote card health: fail_events=%d blocked_cards=%d", failEvents, blockedCards)
	}
}

func TestPublicCDKResultRequiresCurrentAttemptBeforeUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("missing attempt token", func(t *testing.T) {
		openPublicHandlerTestDB(t, true)
		const (
			code  = "PULS-RESULT-MISSING-ATTEMPT"
			token = "result-missing-attempt-token"
		)
		if err := db.BindCDKRedemptionToken(code, token); err != nil {
			t.Fatal(err)
		}
		if err := db.ActivateCDKQueryWindowByToken(token, time.Now().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		var calls atomic.Int32
		upstream := newPublicCardPlatformUpstream(t, gin.H{}, &calls)
		t.Setenv("CARD_API_BASE", upstream.URL)
		router := gin.New()
		router.POST("/result", PublicCDKResult)
		status, response := callPublicHandler(t, router, "/result", gin.H{"token": token})
		if status != http.StatusBadRequest {
			t.Fatalf("missing attempt status=%d response=%v", status, response)
		}
		if calls.Load() != 0 {
			t.Fatalf("missing attempt reached upstream %d times", calls.Load())
		}
	})

	t.Run("old attempt with provider-reused token", func(t *testing.T) {
		database := openPublicHandlerTestDB(t, true)
		const (
			code  = "PULS-RESULT-OLD-ATTEMPT"
			token = "result-reused-provider-token"
		)
		if err := db.BindCDKRedemptionToken(code, token); err != nil {
			t.Fatal(err)
		}
		if err := db.ActivateCDKQueryWindowByToken(token, time.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		oldAttempt, err := db.GetBindingByCDK(code)
		if err != nil {
			t.Fatal(err)
		}
		pastStart := time.Now().UTC().Add(-8 * 24 * time.Hour).Unix()
		if _, err := database.Exec(`
			UPDATE cdk_session_bindings
			SET query_started_at=?, query_expires_at=? WHERE cdk_code=?
		`, pastStart, pastStart+int64(7*24*time.Hour/time.Second), code); err != nil {
			t.Fatal(err)
		}
		expired, err := db.GetBindingByCDK(code)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.BindCDKRedemptionTokenCAS(code, token, expired); err != nil {
			t.Fatal(err)
		}
		if err := db.ActivateCDKQueryWindowByToken(token, time.Now()); err != nil {
			t.Fatal(err)
		}
		current, err := db.GetBindingByCDK(code)
		if err != nil {
			t.Fatal(err)
		}
		if current.AttemptNonce == oldAttempt.AttemptNonce {
			t.Fatal("same provider token did not rotate attempt identity")
		}

		var calls atomic.Int32
		upstream := newPublicCardPlatformUpstream(t, gin.H{}, &calls)
		t.Setenv("CARD_API_BASE", upstream.URL)
		router := gin.New()
		router.POST("/result", PublicCDKResult)
		status, response := callPublicHandler(t, router, "/result", gin.H{
			"token":         token,
			"attempt_token": oldAttempt.AttemptNonce,
		})
		if status != http.StatusNotFound {
			t.Fatalf("old attempt status=%d response=%v", status, response)
		}
		if calls.Load() != 0 {
			t.Fatalf("old attempt reached upstream %d times", calls.Load())
		}
	})
}

type publicHandlerResponse struct {
	status int
	body   map[string]any
	header http.Header
}

func asyncPublicHandlerCall(router http.Handler, path string, body any) <-chan publicHandlerResponse {
	result := make(chan publicHandlerResponse, 1)
	go func() {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Redemption-Device", "public-cas-test")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		response := map[string]any{}
		_ = json.Unmarshal(recorder.Body.Bytes(), &response)
		result <- publicHandlerResponse{status: recorder.Code, body: response, header: recorder.Header().Clone()}
	}()
	return result
}

func TestPublicCDKPreviewCASRejectsConcurrentRedeem(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database := openPublicHandlerTestDB(t, true)
	const (
		code     = "PULS-PREVIEW-RACE-REDEEM"
		oldToken = "preview-race-original-token"
		newToken = "preview-race-stale-response-token"
	)
	if err := db.BindCDKRedemptionToken(code, oldToken); err != nil {
		t.Fatal(err)
	}
	original, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
		INSERT INTO recharge_tasks(task_id, cdk_code, session_json)
		VALUES ('preview-race-current-task', ?, 'current-task-session')
	`, code); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(gin.H{
			"status":           "unused",
			"redemption_token": newToken,
		})
	}))
	defer upstream.Close()
	t.Setenv("CARD_API_BASE", upstream.URL)
	router := gin.New()
	router.POST("/preview", PublicCDKPreview)
	responseCh := asyncPublicHandlerCall(router, "/preview", gin.H{"code": code})
	<-entered
	startedAt := time.Now().UTC().Truncate(time.Second)
	const preflightToken = "preview-race-current-preflight"
	if err := db.RegisterCDKPreflightGrant(code, oldToken, original.AttemptNonce, preflightToken, startedAt); err != nil {
		close(release)
		t.Fatal(err)
	}
	if err := db.ClaimCDKRedemptionAttempt(oldToken, original.AttemptNonce, preflightToken, startedAt); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	response := <-responseCh
	if response.status != http.StatusConflict {
		t.Fatalf("preview-vs-redeem status=%d response=%v", response.status, response.body)
	}
	if calls.Load() != 1 {
		t.Fatalf("preview-vs-redeem upstream calls=%d", calls.Load())
	}
	binding, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if binding.RedemptionToken != oldToken || binding.SubmitClaimedAt != startedAt.Unix() ||
		binding.QueryStartedAt != startedAt.Unix() ||
		binding.QueryExpiresAt == 0 {
		t.Fatalf("stale preview overwrote redeemed binding: %+v", binding)
	}
	var taskSession string
	if err := database.QueryRow(`
		SELECT session_json FROM recharge_tasks WHERE task_id='preview-race-current-task'
	`).Scan(&taskSession); err != nil {
		t.Fatal(err)
	}
	if taskSession != "current-task-session" {
		t.Fatalf("stale preview scrubbed current task session: %q", taskSession)
	}
}

func TestPublicCDKPreviewCASRejectsOlderConcurrentPreview(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openPublicHandlerTestDB(t, true)
	const code = "PULS-PREVIEW-RACE-PREVIEW"
	entered := make(chan int, 2)
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := int(calls.Add(1))
		entered <- call
		if call == 1 {
			<-releaseFirst
			_ = json.NewEncoder(w).Encode(gin.H{
				"status":           "unused",
				"redemption_token": "newer-preview-token",
			})
			return
		}
		<-releaseSecond
		_ = json.NewEncoder(w).Encode(gin.H{
			"status":           "unused",
			"redemption_token": "older-preview-token",
		})
	}))
	defer upstream.Close()
	t.Setenv("CARD_API_BASE", upstream.URL)
	router := gin.New()
	router.POST("/preview", PublicCDKPreview)

	firstResponse := asyncPublicHandlerCall(router, "/preview", gin.H{"code": code})
	if call := <-entered; call != 1 {
		t.Fatalf("first upstream call=%d", call)
	}
	secondResponse := asyncPublicHandlerCall(router, "/preview", gin.H{"code": code})
	if call := <-entered; call != 2 {
		t.Fatalf("second upstream call=%d", call)
	}
	close(releaseFirst)
	first := <-firstResponse
	if first.status != http.StatusOK {
		close(releaseSecond)
		t.Fatalf("newer preview status=%d response=%v", first.status, first.body)
	}
	close(releaseSecond)
	second := <-secondResponse
	if second.status != http.StatusConflict {
		t.Fatalf("older preview status=%d response=%v", second.status, second.body)
	}
	binding, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if binding.RedemptionToken != "newer-preview-token" || binding.QueryStartedAt != 0 ||
		binding.QueryExpiresAt != 0 {
		t.Fatalf("older preview overwrote newer preview: %+v", binding)
	}
}
