package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
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

const completionBridgeTestSecret = "0123456789abcdef0123456789abcdef"

func signedMarketplaceBindCall(t *testing.T, router *gin.Engine, body marketplaceCompletionBindRequest, purpose string, stamp int64) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/internal/local-cdk/bind", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	stampText := strconvFormatInt(stamp)
	request.Header.Set("X-MaplePass-Time", stampText)
	request.Header.Set("X-MaplePass-Signature", completionBridgeSignature(completionBridgeTestSecret, purpose, stampText, raw))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var data map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &data)
	return response.Code, data
}

// strconvFormatInt keeps test request construction clear without leaking any
// real credential into fixture data.
func strconvFormatInt(value int64) string { return strconv.FormatInt(value, 10) }

func installMarketplaceBindRoute(f *localFixture) {
	f.router.POST("/internal/local-cdk/bind", MarketplaceLocalCDKBind)
}

func TestMarketplaceLocalCDKBindIsSignedIdempotentAndCannotRebind(t *testing.T) {
	f := newLocalFixture(t)
	installMarketplaceBindRoute(f)
	t.Setenv("CDK_SSO_SHARED_SECRET", completionBridgeTestSecret)
	code := f.code(t)
	codeHash := localHash(code)
	orderA := "018f27ef-7a39-7e91-89ab-cdef01234567"
	orderB := "018f27ef-7a39-7e91-89ab-cdef01234568"
	now := time.Now().UnixMilli()

	status, body := signedMarketplaceBindCall(t, f.router, marketplaceCompletionBindRequest{CodeHash: codeHash, OrderID: orderA}, marketplaceCompletionBindPurpose, now)
	if status != http.StatusOK || body["alreadyBound"] != false {
		t.Fatalf("first bind: status=%d body=%v", status, body)
	}
	status, body = signedMarketplaceBindCall(t, f.router, marketplaceCompletionBindRequest{CodeHash: codeHash, OrderID: strings.ToUpper(orderA)}, marketplaceCompletionBindPurpose, now+1)
	if status != http.StatusOK || body["alreadyBound"] != true {
		t.Fatalf("same bind must be idempotent: status=%d body=%v", status, body)
	}
	status, _ = signedMarketplaceBindCall(t, f.router, marketplaceCompletionBindRequest{CodeHash: codeHash, OrderID: orderB}, marketplaceCompletionBindPurpose, now+2)
	if status != http.StatusConflict {
		t.Fatalf("rebind status=%d, want %d", status, http.StatusConflict)
	}
	var count int
	var storedOrder string
	if err := db.DB.QueryRow("SELECT COUNT(*),MAX(marketplace_order_id) FROM marketplace_local_cdk_bindings").Scan(&count, &storedOrder); err != nil {
		t.Fatal(err)
	}
	if count != 1 || storedOrder != orderA {
		t.Fatalf("binding overwritten: count=%d order=%q", count, storedOrder)
	}

	secondHash := strings.Repeat("c", 64)
	if _, err := db.DB.Exec(`INSERT INTO local_cdks(code_hash,prefix,plan,status,expires_at,created_at)
		VALUES(?,'MAPLE-OTHER','pro_20x','unused',?,?)`, secondHash, time.Now().Unix()+86400, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	status, _ = signedMarketplaceBindCall(t, f.router, marketplaceCompletionBindRequest{CodeHash: secondHash, OrderID: orderA}, marketplaceCompletionBindPurpose, now+3)
	if status != http.StatusConflict {
		t.Fatalf("same marketplace order must not bind another code: status=%d", status)
	}
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM marketplace_local_cdk_bindings").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("one marketplace order created %d bindings", count)
	}
}

func TestMarketplaceLocalCDKBindRejectsTamperedAndStaleRequest(t *testing.T) {
	f := newLocalFixture(t)
	installMarketplaceBindRoute(f)
	t.Setenv("CDK_SSO_SHARED_SECRET", completionBridgeTestSecret)
	code := f.code(t)
	codeHash := localHash(code)
	order := "018f27ef-7a39-7e91-89ab-cdef01234567"
	raw, _ := json.Marshal(marketplaceCompletionBindRequest{CodeHash: codeHash, OrderID: order})
	stamp := strconvFormatInt(time.Now().UnixMilli())
	request := httptest.NewRequest(http.MethodPost, "/internal/local-cdk/bind", bytes.NewReader(raw))
	request.Header.Set("X-MaplePass-Time", stamp)
	request.Header.Set("X-MaplePass-Signature", completionBridgeSignature(completionBridgeTestSecret, marketplaceCompletionDeliveryPurpose, stamp, raw))
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong purpose status=%d", response.Code)
	}

	status, _ := signedMarketplaceBindCall(t, f.router, marketplaceCompletionBindRequest{CodeHash: codeHash, OrderID: order}, marketplaceCompletionBindPurpose, time.Now().Add(-6*time.Minute).UnixMilli())
	if status != http.StatusUnauthorized {
		t.Fatalf("stale request status=%d", status)
	}
	var count int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM marketplace_local_cdk_bindings").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid requests wrote %d bindings", count)
	}
}

func TestLateMarketplaceBindingQueuesAlreadyCompletedCode(t *testing.T) {
	f := newLocalFixture(t)
	installMarketplaceBindRoute(f)
	t.Setenv("CDK_SSO_SHARED_SECRET", completionBridgeTestSecret)
	codeHash := strings.Repeat("b", 64)
	now := time.Now().Unix()
	if _, err := db.DB.Exec(`INSERT INTO local_cdks(id,code_hash,prefix,plan,status,expires_at,created_at,activated_at,upstream_completion_verified_at,upstream_completion_source)
		VALUES(1,?,'MAPLE-LATE','pro_20x','consumed',?,?,?,?,'cardplatform_direct_order')`, codeHash, now+86400, now, now-60, now-60); err != nil {
		t.Fatal(err)
	}
	orderID := "018f27ef-7a39-7e91-89ab-cdef01234567"
	status, body := signedMarketplaceBindCall(t, f.router, marketplaceCompletionBindRequest{CodeHash: codeHash, OrderID: orderID}, marketplaceCompletionBindPurpose, time.Now().UnixMilli())
	if status != http.StatusOK {
		t.Fatalf("late bind: status=%d body=%v", status, body)
	}
	var storedOrder string
	var outboxCount int
	if err := db.DB.QueryRow("SELECT marketplace_order_id FROM marketplace_local_cdk_bindings WHERE local_id=1").Scan(&storedOrder); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM marketplace_completion_outbox WHERE local_id=1").Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if storedOrder != orderID || outboxCount != 1 {
		t.Fatalf("late bind did not queue safely: order=%q outbox=%d", storedOrder, outboxCount)
	}
}

func insertBoundPro20ForCompletion(t *testing.T) (*localFixture, string, string) {
	t.Helper()
	f := newLocalFixture(t)
	installMarketplaceBindRoute(f)
	t.Setenv("CDK_SSO_SHARED_SECRET", completionBridgeTestSecret)
	codeHash := strings.Repeat("a", 64)
	now := time.Now().Unix()
	if _, err := db.DB.Exec(`INSERT INTO local_cdks(id,code_hash,prefix,plan,status,expires_at,created_at)
		VALUES(1,?,'MAPLE-TEST','pro_20x','reserved',?,?)`, codeHash, now+86400, now); err != nil {
		t.Fatal(err)
	}
	orderID := "018f27ef-7a39-7e91-89ab-cdef01234567"
	status, body := signedMarketplaceBindCall(t, f.router, marketplaceCompletionBindRequest{CodeHash: codeHash, OrderID: orderID}, marketplaceCompletionBindPurpose, time.Now().UnixMilli())
	if status != http.StatusOK {
		t.Fatalf("bind: status=%d body=%v", status, body)
	}
	return f, codeHash, orderID
}

func TestPro20CompletionCreatesOneMinimalSignedOutboxEvent(t *testing.T) {
	_, codeHash, orderID := insertBoundPro20ForCompletion(t)
	completedAt := "2026-09-19T15:21:45Z"
	now := time.Now().Unix()
	if err := recordAuthoritativeLocalCompletion(1, "会员已开通，卡密已核销", completedAt, now); err != nil {
		t.Fatal(err)
	}
	if err := recordAuthoritativeLocalCompletion(1, "会员已开通，卡密已核销", completedAt, now+1); err != nil {
		t.Fatal(err)
	}
	var state, upgradeType string
	var activatedAt, expiryAt int64
	if err := db.DB.QueryRow("SELECT status,upgrade_type,activated_at,subscription_expires_at FROM local_cdks WHERE id=1").Scan(&state, &upgradeType, &activatedAt, &expiryAt); err != nil {
		t.Fatal(err)
	}
	if state != "consumed" || upgradeType != "ChatGPT Pro 20X" || activatedAt == 0 || expiryAt <= activatedAt {
		t.Fatalf("completion details missing: state=%q type=%q activated=%d expiry=%d", state, upgradeType, activatedAt, expiryAt)
	}
	var count int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM marketplace_completion_outbox WHERE local_id=1").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("outbox count=%d, want 1", count)
	}

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		stamp := r.Header.Get("X-MaplePass-Time")
		mac := hmac.New(sha256.New, []byte(completionBridgeTestSecret))
		_, _ = mac.Write([]byte(marketplaceCompletionDeliveryPurpose + ":" + stamp + ":" + string(raw)))
		if got, want := r.Header.Get("X-MaplePass-Signature"), hex.EncodeToString(mac.Sum(nil)); got != want {
			t.Errorf("bad completion signature: got=%q want=%q", got, want)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(fields) != 7 {
			t.Errorf("unexpected completion fields: %v", fields)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, forbidden := range []string{"email", "telegram", "session", "credential", "code", "upstreamOrder"} {
			if _, exists := fields[forbidden]; exists {
				t.Errorf("forbidden field %q sent", forbidden)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		var event marketplaceCompletionEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if event.EventID != "local:1:completed" || event.OrderID != orderID || event.LocalID != 1 || event.CodeHash != codeHash || event.Plan != "pro_20x" || event.CompletedAt != completedAt {
			t.Errorf("bad completion event: %#v", event)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("MAPLE_STORE_COMPLETION_WEBHOOK_URL", server.URL)
	dispatchMarketplaceCompletionOutbox(context.Background())
	dispatchMarketplaceCompletionOutbox(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("completion delivery calls=%d, want 1", calls.Load())
	}
	var outboxState string
	if err := db.DB.QueryRow("SELECT state FROM marketplace_completion_outbox WHERE local_id=1").Scan(&outboxState); err != nil {
		t.Fatal(err)
	}
	if outboxState != "sent" {
		t.Fatalf("outbox state=%q", outboxState)
	}
}

func TestMarketplaceCompletionCostRequiresAuthoritativeCompleteSnapshot(t *testing.T) {
	complete := `{"order":{"status":"completed","total_cost_usd_minor":1716,"subscription_cost_usd_minor":1684,"service_fee_minor":15,"recharge_fee_usd_minor":16,"open_fee_allocated_usd_minor":1,"cost_complete":true,"cost_source":"zovo_reconciled"}}`
	cost := marketplaceCompletionCostFromSnapshot(complete)
	if cost == nil || cost.TotalUSDMinor != 1716 || cost.SubscriptionUSDMinor != 1684 || cost.ServiceFeeUSDMinor != 15 || cost.RechargeFeeUSDMinor != 16 || cost.OpenFeeAllocatedUSDMinor != 1 || !cost.Complete || cost.Source != "zovo_reconciled" {
		t.Fatalf("unexpected authoritative cost: %#v", cost)
	}
	partial := `{"order":{"funding_card_amount_minor":1684,"service_fee_minor":15}}`
	if cost := marketplaceCompletionCostFromSnapshot(partial); cost != nil {
		t.Fatalf("partial estimate must not be exported: %#v", cost)
	}
}

func TestDeliveredCompletionBackfillsAuthoritativeCostWithSameEvent(t *testing.T) {
	_, _, _ = insertBoundPro20ForCompletion(t)
	if err := recordAuthoritativeLocalCompletion(1, "会员已开通，卡密已核销", "2026-09-19T15:21:45Z", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var last marketplaceCompletionEvent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := json.NewDecoder(r.Body).Decode(&last); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("MAPLE_STORE_COMPLETION_WEBHOOK_URL", server.URL)
	dispatchMarketplaceCompletionOutbox(context.Background())
	if calls.Load() != 1 || last.Cost != nil {
		t.Fatalf("initial completion calls=%d cost=%#v", calls.Load(), last.Cost)
	}
	snapshot := `{"order":{"total_cost_usd_minor":1716,"subscription_cost_usd_minor":1684,"service_fee_minor":15,"recharge_fee_usd_minor":16,"open_fee_allocated_usd_minor":1,"cost_complete":true,"cost_source":"zovo_reconciled"}}`
	if _, err := db.DB.Exec(`INSERT INTO automation_watch(local_id,snapshot) VALUES(1,?)`, snapshot); err != nil {
		t.Fatal(err)
	}
	dispatchMarketplaceCompletionOutbox(context.Background())
	if calls.Load() != 2 || last.EventID != "local:1:completed" || last.Cost == nil || last.Cost.TotalUSDMinor != 1716 {
		t.Fatalf("historical cost was not backfilled safely: calls=%d event=%#v", calls.Load(), last)
	}
	var marker string
	if err := db.DB.QueryRow(`SELECT last_error FROM marketplace_completion_outbox WHERE local_id=1`).Scan(&marker); err != nil || marker != "cost_synced" {
		t.Fatalf("cost sync marker=%q err=%v", marker, err)
	}
}

func TestCompletionOutboxRetriesAfterFailureWithoutDuplicatingRows(t *testing.T) {
	_, _, _ = insertBoundPro20ForCompletion(t)
	if err := recordAuthoritativeLocalCompletion(1, "会员已开通，卡密已核销", "2026-09-19T15:21:45Z", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("MAPLE_STORE_COMPLETION_WEBHOOK_URL", server.URL)
	dispatchMarketplaceCompletionOutbox(context.Background())
	var state string
	var attempts int
	if err := db.DB.QueryRow("SELECT state,attempts FROM marketplace_completion_outbox WHERE local_id=1").Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || attempts != 1 {
		t.Fatalf("first failure state=%q attempts=%d", state, attempts)
	}
	if _, err := db.DB.Exec("UPDATE marketplace_completion_outbox SET next_attempt_at=0 WHERE local_id=1"); err != nil {
		t.Fatal(err)
	}
	dispatchMarketplaceCompletionOutbox(context.Background())
	if calls.Load() != 2 {
		t.Fatalf("retry calls=%d, want 2", calls.Load())
	}
	var count int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM marketplace_completion_outbox WHERE local_id=1 AND state='sent'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("sent outbox rows=%d", count)
	}
}

func TestCompletionOutboxRecoversExpiredSendingLease(t *testing.T) {
	_, _, _ = insertBoundPro20ForCompletion(t)
	if err := recordAuthoritativeLocalCompletion(1, "会员已开通，卡密已核销", "2026-09-19T15:21:45Z", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec("UPDATE marketplace_completion_outbox SET state='sending',attempts=1,lease_until=? WHERE local_id=1", time.Now().Unix()-1); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("MAPLE_STORE_COMPLETION_WEBHOOK_URL", server.URL)
	dispatchMarketplaceCompletionOutbox(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("recovered lease calls=%d, want 1", calls.Load())
	}
	var state string
	if err := db.DB.QueryRow("SELECT state FROM marketplace_completion_outbox WHERE local_id=1").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "sent" {
		t.Fatalf("expired lease did not recover: state=%q", state)
	}
}

func TestCompletionTransitionRollsBackWhenOutboxInsertFails(t *testing.T) {
	_, _, _ = insertBoundPro20ForCompletion(t)
	if _, err := db.DB.Exec(`CREATE TRIGGER reject_marketplace_completion_outbox
		BEFORE INSERT ON marketplace_completion_outbox
		BEGIN SELECT RAISE(ABORT, 'outbox unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	err := recordAuthoritativeLocalCompletion(1, "会员已开通，卡密已核销", "2026-09-19T15:21:45Z", time.Now().Unix())
	if err == nil {
		t.Fatal("completion must fail when its durable outbox cannot be written")
	}
	var state string
	var activatedAt int64
	if err := db.DB.QueryRow("SELECT status,activated_at FROM local_cdks WHERE id=1").Scan(&state, &activatedAt); err != nil {
		t.Fatal(err)
	}
	if state != "reserved" || activatedAt != 0 {
		t.Fatalf("transition was partially committed: state=%q activated=%d", state, activatedAt)
	}
	var count int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM marketplace_completion_outbox WHERE local_id=1").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("outbox wrote %d rows despite trigger", count)
	}
}
