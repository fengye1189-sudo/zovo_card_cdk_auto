package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

const officialInvoiceTestSecret = "0123456789abcdef0123456789abcdef"

func signedMarketplaceOfficialInvoiceCall(t *testing.T, router *gin.Engine, body marketplaceOfficialInvoiceRequest, purpose string) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	request := httptest.NewRequest(http.MethodPost, "/internal/local-cdk/official-invoice", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-MaplePass-Time", stamp)
	request.Header.Set("X-MaplePass-Signature", completionBridgeSignature(officialInvoiceTestSecret, purpose, stamp, raw))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var payload map[string]interface{}
	_ = json.Unmarshal(response.Body.Bytes(), &payload)
	return response, payload
}

func installMarketplaceOfficialInvoiceRoute(f *localFixture) {
	f.router.POST("/internal/local-cdk/official-invoice", MarketplaceOfficialInvoice)
}

func insertAuthoritativeMarketplaceInvoiceFixture(t *testing.T) (*localFixture, marketplaceOfficialInvoiceRequest, string, int64) {
	t.Helper()
	f := newLocalFixture(t)
	installMarketplaceOfficialInvoiceRoute(f)
	t.Setenv("CDK_SSO_SHARED_SECRET", officialInvoiceTestSecret)
	const (
		localID = int64(1)
		orderID = "018f27ef-7a39-7e91-89ab-cdef01234567"
		codeHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		email    = "invoice.customer@example.com"
	)
	completedAt := time.Now().Unix()
	if _, err := db.DB.Exec(`INSERT INTO local_cdks
		(id,code_hash,prefix,plan,status,expires_at,created_at,email,activated_at,upstream_completion_verified_at,upstream_completion_source)
		VALUES(?,?,'MAPLE-INVOICE','pro_20x','consumed',?,?,?,?,?,?)`,
		localID, codeHash, completedAt+86400, completedAt-300, email, completedAt, completedAt, "cardplatform_direct_order"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO marketplace_local_cdk_bindings
		(local_id,code_hash,marketplace_order_id,created_at) VALUES(?,?,?,?)`,
		localID, codeHash, orderID, completedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO marketplace_completion_outbox
		(event_id,local_id,code_hash,marketplace_order_id,plan,completed_at,state,attempts,next_attempt_at,lease_until,last_error,created_at,updated_at)
		VALUES('local:1:completed',?,?,?,?,?,'sending',0,0,0,'',?,?)`,
		localID, codeHash, orderID, "pro_20x", completedAt, completedAt, completedAt); err != nil {
		t.Fatal(err)
	}
	return f, marketplaceOfficialInvoiceRequest{OrderID: orderID, CodeHash: codeHash, LocalID: localID}, email, completedAt
}

func TestMarketplaceOfficialInvoiceReturnsOnlyOneMatchedPaidIssuerInvoice(t *testing.T) {
	f, requestBody, email, completedAt := insertAuthoritativeMarketplaceInvoiceFixture(t)
	var lookupCalls atomic.Int32
	accountHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookupCalls.Add(1)
		if r.URL.Path != "/gpt/invoices-by-email" {
			t.Errorf("unexpected account hub path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("email"); got != email {
			t.Errorf("account hub email=%q", got)
		}
		_ = json.NewEncoder(w).Encode(gin.H{"invoices": []gin.H{
			{
				"number":             "outside-window",
				"paid":               true,
				"created":            completedAt + int64(3*time.Hour/time.Second),
				"invoice_pdf":        "https://pay.stripe.com/other.pdf",
				"customer_email":     email,
			},
			{
				"number":             "openai-inv-123",
				"status":             "paid",
				"created":            completedAt + 45,
				"hosted_invoice_url": "https://invoice.stripe.com/i/openai-inv-123",
				"invoice_pdf":        "https://pay.stripe.com/i/openai-inv-123.pdf",
				"customer_email":     email,
				"metadata":           gin.H{"private": "do-not-forward"},
			},
		}})
	}))
	defer accountHub.Close()
	t.Setenv("ACCOUNTHUB_BASE_URL", accountHub.URL)

	response, payload := signedMarketplaceOfficialInvoiceCall(t, f.router, requestBody, marketplaceOfficialInvoicePurpose)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "private, no-store, max-age=0" {
		t.Fatalf("cache header=%q", response.Header().Get("Cache-Control"))
	}
	if payload["status"] != "READY" || payload["invoiceNumber"] != "openai-inv-123" {
		t.Fatalf("response=%v", payload)
	}
	if payload["hostedInvoiceUrl"] != "https://invoice.stripe.com/i/openai-inv-123" || payload["invoicePdfUrl"] != "https://pay.stripe.com/i/openai-inv-123.pdf" {
		t.Fatalf("issuer urls missing: %v", payload)
	}
	if lookupCalls.Load() != 1 {
		t.Fatalf("lookup calls=%d", lookupCalls.Load())
	}
	encoded := response.Body.String()
	for _, forbidden := range []string{email, "MAPLE-INVOICE", "do-not-forward", "outside-window"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestMarketplaceOfficialInvoiceRejectsWrongSignatureBeforeLookup(t *testing.T) {
	f, requestBody, _, _ := insertAuthoritativeMarketplaceInvoiceFixture(t)
	var lookupCalls atomic.Int32
	accountHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookupCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer accountHub.Close()
	t.Setenv("ACCOUNTHUB_BASE_URL", accountHub.URL)

	response, _ := signedMarketplaceOfficialInvoiceCall(t, f.router, requestBody, marketplaceCompletionDeliveryPurpose)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-purpose status=%d", response.Code)
	}
	if lookupCalls.Load() != 0 {
		t.Fatalf("unauthorized lookup calls=%d", lookupCalls.Load())
	}
}

func TestMarketplaceOfficialInvoiceFailsClosedForWrongLocalOrNonAuthoritativeCompletion(t *testing.T) {
	f, requestBody, _, _ := insertAuthoritativeMarketplaceInvoiceFixture(t)
	var lookupCalls atomic.Int32
	accountHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookupCalls.Add(1)
		_ = json.NewEncoder(w).Encode(gin.H{"invoices": []gin.H{}})
	}))
	defer accountHub.Close()
	t.Setenv("ACCOUNTHUB_BASE_URL", accountHub.URL)

	wrongLocal := requestBody
	wrongLocal.LocalID++
	response, payload := signedMarketplaceOfficialInvoiceCall(t, f.router, wrongLocal, marketplaceOfficialInvoicePurpose)
	if response.Code != http.StatusOK || payload["status"] != "UNAVAILABLE" {
		t.Fatalf("wrong local response=%d %v", response.Code, payload)
	}
	if lookupCalls.Load() != 0 {
		t.Fatalf("wrong local queried account hub")
	}

	// A historical/imported consumed state is not an upstream proof. It must
	// never be turned into an invoice grant by the presence of an outbox row.
	if _, err := db.DB.Exec(`UPDATE local_cdks
		SET upstream_completion_verified_at=0,upstream_completion_source='' WHERE id=?`, requestBody.LocalID); err != nil {
		t.Fatal(err)
	}
	response, payload = signedMarketplaceOfficialInvoiceCall(t, f.router, requestBody, marketplaceOfficialInvoicePurpose)
	if response.Code != http.StatusOK || payload["status"] != "UNAVAILABLE" {
		t.Fatalf("unverified consumed response=%d %v", response.Code, payload)
	}
	if lookupCalls.Load() != 0 {
		t.Fatalf("unverified consumed record queried account hub")
	}

	if _, err := db.DB.Exec("UPDATE local_cdks SET status='review',activated_at=0 WHERE id=?", requestBody.LocalID); err != nil {
		t.Fatal(err)
	}
	response, payload = signedMarketplaceOfficialInvoiceCall(t, f.router, requestBody, marketplaceOfficialInvoicePurpose)
	if response.Code != http.StatusOK || payload["status"] != "UNAVAILABLE" {
		t.Fatalf("non-authoritative response=%d %v", response.Code, payload)
	}
	if lookupCalls.Load() != 0 {
		t.Fatalf("non-authoritative completion queried account hub")
	}
}

func TestMarketplaceOfficialInvoiceNeverChoosesAmbiguousOrUntrustedCandidate(t *testing.T) {
	f, requestBody, _, completedAt := insertAuthoritativeMarketplaceInvoiceFixture(t)
	for _, tc := range []struct {
		name     string
		invoices []gin.H
		want     string
	}{
		{
			name: "not-generated-yet",
			invoices: []gin.H{{
				"number": "unpaid", "status": "open", "created": completedAt + 10,
				"invoice_pdf": "https://pay.stripe.com/unpaid.pdf",
			}},
			want: "PENDING",
		},
		{
			name: "contradictory-unpaid-flag",
			invoices: []gin.H{{
				"number": "contradictory", "paid": false, "status": "paid", "created": completedAt + 10,
				"invoice_pdf": "https://pay.stripe.com/contradictory.pdf",
			}},
			want: "PENDING",
		},
		{
			name: "two-paid-invoices",
			invoices: []gin.H{
				{"number": "first", "paid": true, "created": completedAt + 10, "invoice_pdf": "https://pay.stripe.com/first.pdf"},
				{"number": "second", "paid": true, "created": completedAt + 20, "invoice_pdf": "https://pay.stripe.com/second.pdf"},
			},
			want: "UNAVAILABLE",
		},
		{
			name: "same-number-conflicting-urls",
			invoices: []gin.H{
				{"number": "same", "paid": true, "created": completedAt + 10, "invoice_pdf": "https://pay.stripe.com/first.pdf"},
				{"number": "same", "paid": true, "created": completedAt + 10, "invoice_pdf": "https://pay.stripe.com/second.pdf"},
			},
			want: "UNAVAILABLE",
		},
		{
			name: "lookalike-host-rejected",
			invoices: []gin.H{{
				"number": "lookalike", "paid": true, "created": completedAt + 10,
				"invoice_pdf": "https://notstripe.com/lookalike.pdf",
			}},
			want: "UNAVAILABLE",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accountHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(gin.H{"invoices": tc.invoices})
			}))
			defer accountHub.Close()
			t.Setenv("ACCOUNTHUB_BASE_URL", accountHub.URL)
			response, payload := signedMarketplaceOfficialInvoiceCall(t, f.router, requestBody, marketplaceOfficialInvoicePurpose)
			if response.Code != http.StatusOK || payload["status"] != tc.want {
				t.Fatalf("response=%d %v", response.Code, payload)
			}
			if _, exists := payload["hostedInvoiceUrl"]; exists {
				t.Fatalf("non-ready response exposed hosted invoice: %v", payload)
			}
			if _, exists := payload["invoicePdfUrl"]; exists {
				t.Fatalf("non-ready response exposed PDF invoice: %v", payload)
			}
		})
	}
}

func TestPublicInvoiceMetadataRemovesIssuerURLsAndCustomerIdentity(t *testing.T) {
	got := publicInvoiceMetadata([]map[string]interface{}{{
		"number":             "openai-inv-123",
		"status":             "paid",
		"paid":               true,
		"currency":           "usd",
		"total":              2000,
		"amount_paid":        2000,
		"created":            2_000_000_000,
		// Descriptions are untrusted free text. A provider could place a URL or
		// email there, so public metadata deliberately omits it altogether.
		"description":        "https://invoice.stripe.com/private for invoice.customer@example.com",
		"hosted_invoice_url": "https://invoice.stripe.com/private",
		"invoice_pdf":        "https://pay.stripe.com/private.pdf",
		"customer_email":     "invoice.customer@example.com",
		"metadata":           gin.H{"secret": "private"},
		"nested_total":       gin.H{"url": "https://stripe.com/private"},
	}})
	if len(got) != 1 {
		t.Fatalf("metadata=%v", got)
	}
	encoded, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"stripe.com", "invoice.customer@example.com", "metadata", "private", "description", "nested_total"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("public invoice metadata leaked %q: %s", forbidden, encoded)
		}
	}
	if string(encoded) == "{}" || !strings.Contains(string(encoded), "openai-inv-123") {
		t.Fatalf("safe metadata missing: %s", encoded)
	}
}

func TestPublicInvoiceMetadataRejectsNonScalarOrSensitiveNumberFields(t *testing.T) {
	got := publicInvoiceMetadata([]map[string]interface{}{{
		"number":      "https://stripe.com/private @buyer@example.com",
		"status":      gin.H{"paid": true},
		"currency":    gin.H{"value": "usd"},
		"total":       gin.H{"amount": 2000},
		"amount_paid": json.Number("2000"),
		"created":     gin.H{"unix": 2_000_000_000},
		"paid":        true,
	}})
	if len(got) != 1 {
		t.Fatalf("metadata=%v", got)
	}
	encoded, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"stripe.com", "buyer@example.com", "number", "status", "currency", "total", "created"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("public invoice metadata leaked %q: %s", forbidden, encoded)
		}
	}
	if string(encoded) != `{"paid":true,"amount_paid":2000}` && string(encoded) != `{"amount_paid":2000,"paid":true}` {
		t.Fatalf("unexpected safe scalar metadata: %s", encoded)
	}
}

func TestTrustedMarketplaceIssuerURLRejectsNonstandardPort(t *testing.T) {
	if got := trustedMarketplaceIssuerURL("https://pay.stripe.com:8443/private.pdf"); got != "" {
		t.Fatalf("nonstandard issuer port was accepted: %q", got)
	}
	if got := trustedMarketplaceIssuerURL("https://pay.stripe.com:443/private.pdf"); got == "" {
		t.Fatal("standard HTTPS port was rejected")
	}
}

func TestMarketplaceBindingCannotBackfillHistoricalConsumedCode(t *testing.T) {
	newLocalFixture(t)
	const (
		localID = int64(97)
		codeHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		orderID = "018f27ef-7a39-7e91-89ab-cdef01234568"
	)
	now := time.Now().Unix()
	if _, err := db.DB.Exec(`INSERT INTO local_cdks
		(id,code_hash,prefix,plan,status,expires_at,created_at,activated_at)
		VALUES(?,?,'HISTORICAL','pro_20x','consumed',?,?,?)`,
		localID, codeHash, now+86400, now-86400, now-3600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bindMarketplaceLocalCDK(codeHash, orderID, now); err != errMarketplaceBindingConflict {
		t.Fatalf("late historical binding error=%v, want binding conflict", err)
	}
	var count int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM marketplace_local_cdk_bindings WHERE local_id=?", localID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("historical consumed code was bound: %d", count)
	}
}

func TestAuthoritativeLocalCompletionPersistsInvoiceProof(t *testing.T) {
	newLocalFixture(t)
	const localID = int64(98)
	now := time.Now().Unix()
	if _, err := db.DB.Exec(`INSERT INTO local_cdks
		(id,code_hash,prefix,plan,status,expires_at,created_at)
		VALUES(?,?,'AUTHORITY','pro_20x','reserved',?,?)`,
		localID, strings.Repeat("c", 64), now+86400, now); err != nil {
		t.Fatal(err)
	}
	if err := recordAuthoritativeLocalCompletion(localID, "complete", time.Unix(now, 0).UTC().Format(time.RFC3339), now); err != nil {
		t.Fatal(err)
	}
	var status, source string
	var verifiedAt int64
	if err := db.DB.QueryRow(`SELECT status,upstream_completion_verified_at,upstream_completion_source
		FROM local_cdks WHERE id=?`, localID).Scan(&status, &verifiedAt, &source); err != nil {
		t.Fatal(err)
	}
	if status != "consumed" || verifiedAt != now || source != "cardplatform_direct_order" {
		t.Fatalf("completion proof=%q %d %q", status, verifiedAt, source)
	}
}

func TestPublicBillingCheckDoesNotExposeIssuerURLsOrFullEmail(t *testing.T) {
	f := newLocalFixture(t)
	f.router.POST("/billing", SessionBillingCheck)
	const (
		code  = "PULS-PUBLIC-BILLING"
		token = "public-billing-token"
		email = "public.billing@example.com"
	)
	if _, err := db.DB.Exec(`CREATE TABLE cdk_session_bindings (
		cdk_code TEXT PRIMARY KEY, session_payload TEXT NOT NULL,
		redemption_token TEXT, account_email TEXT NOT NULL DEFAULT '',
		attempt_nonce TEXT NOT NULL DEFAULT '', submit_claimed_at INTEGER NOT NULL DEFAULT 0,
		query_started_at INTEGER NOT NULL DEFAULT 0, query_expires_at INTEGER NOT NULL DEFAULT 0,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatal(err)
	}
	if err := db.BindCDKSession(code, token, `{"user":{"email":"`+email+`"}}`); err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateCDKQueryWindowByToken(token, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	accountHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("email"); got != email {
			t.Errorf("account hub email=%q", got)
		}
		_ = json.NewEncoder(w).Encode(gin.H{"invoices": []gin.H{{
			"number":             "openai-public-1",
			"status":             "paid",
			"created":            time.Now().Unix(),
			"hosted_invoice_url": "https://invoice.stripe.com/private",
			"invoice_pdf":        "https://pay.stripe.com/private.pdf",
			"customer_email":     email,
		}}})
	}))
	defer accountHub.Close()
	t.Setenv("ACCOUNTHUB_BASE_URL", accountHub.URL)

	body, err := json.Marshal(gin.H{"cdk_code": code})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/billing", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, forbidden := range []string{"stripe.com", email, "hosted_invoice_url", "invoice_pdf"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("public billing response leaked %q: %s", forbidden, response.Body.String())
		}
	}
	if !strings.Contains(response.Body.String(), "openai-public-1") {
		t.Fatalf("public billing response lost safe invoice metadata: %s", response.Body.String())
	}
}
