package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func signedMarketplaceIssueCall(t *testing.T, f *localFixture, body marketplaceCDKIssueRequest) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	request := httptest.NewRequest(http.MethodPost, "/internal/local-cdk/issue", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-MaplePass-Time", stamp)
	request.Header.Set("X-MaplePass-Signature", completionBridgeSignature(completionBridgeTestSecret, marketplaceCDKIssuePurpose, stamp, raw))
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	var result map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &result)
	return response.Code, result
}

func TestMarketplaceLocalCDKIssueIsSignedPlanBoundAndReplayable(t *testing.T) {
	f := newLocalFixture(t)
	f.router.POST("/internal/local-cdk/issue", MarketplaceLocalCDKIssue)
	t.Setenv("CDK_SSO_SHARED_SECRET", completionBridgeTestSecret)
	orderID := "018f27ef-7a39-7e91-89ab-cdef01234567"
	request := marketplaceCDKIssueRequest{OrderID: orderID, ProductID: "credit500", Count: 2}
	status, first := signedMarketplaceIssueCall(t, f, request)
	if status != http.StatusOK || first["reused"] != false || first["plan"] != "credit500" {
		t.Fatalf("first issue: status=%d body=%v", status, first)
	}
	firstCodes, ok := first["codes"].([]any)
	if !ok || len(firstCodes) != 2 {
		t.Fatalf("first codes=%v", first["codes"])
	}
	status, second := signedMarketplaceIssueCall(t, f, request)
	if status != http.StatusOK || second["reused"] != true {
		t.Fatalf("replay: status=%d body=%v", status, second)
	}
	secondCodes := second["codes"].([]any)
	if firstCodes[0] != secondCodes[0] || firstCodes[1] != secondCodes[1] {
		t.Fatalf("replay changed codes: first=%v second=%v", firstCodes, secondCodes)
	}
	var rows, bindings int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM local_cdks WHERE batch_id=?", "marketplace-"+orderID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM operations_product_bindings b
		JOIN operations_products p ON p.id=b.product_id WHERE p.id='credit500'`).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || bindings != 2 {
		t.Fatalf("rows=%d bindings=%d", rows, bindings)
	}
	status, _ = signedMarketplaceIssueCall(t, f, marketplaceCDKIssueRequest{OrderID: orderID, ProductID: "credit1000", Count: 2})
	if status != http.StatusConflict {
		t.Fatalf("changed product status=%d", status)
	}
}

func TestMarketplaceLocalCDKIssueRejectsUnsignedRequest(t *testing.T) {
	f := newLocalFixture(t)
	f.router.POST("/internal/local-cdk/issue", MarketplaceLocalCDKIssue)
	t.Setenv("CDK_SSO_SHARED_SECRET", completionBridgeTestSecret)
	raw, _ := json.Marshal(marketplaceCDKIssueRequest{OrderID: "018f27ef-7a39-7e91-89ab-cdef01234567", ProductID: "plus", Count: 1})
	request := httptest.NewRequest(http.MethodPost, "/internal/local-cdk/issue", bytes.NewReader(raw))
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned status=%d", response.Code)
	}
}
