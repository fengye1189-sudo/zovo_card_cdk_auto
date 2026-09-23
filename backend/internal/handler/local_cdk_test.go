package handler

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

type localFixture struct {
	plan          string
	quoteAmount   int64
	quoteCurrency string
	router        *gin.Engine
	calls         atomic.Int32
	mode          string
	request       string
	mu            sync.Mutex
	poolMode      string
	paidCards     []int64
}

func newLocalFixture(t *testing.T) *localFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	old := db.DB
	conn, e := sql.Open("sqlite3", filepath.Join(t.TempDir(), "test.db")+"?_busy_timeout=5000&_journal_mode=WAL")
	if e != nil {
		t.Fatal(e)
	}
	db.DB = conn
	t.Cleanup(func() { conn.Close(); db.DB = old })
	_, e = conn.Exec("CREATE TABLE site_settings(key TEXT PRIMARY KEY,value TEXT NOT NULL,updated_at DATETIME DEFAULT CURRENT_TIMESTAMP); CREATE TABLE admin_audit_logs(id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT, action TEXT NOT NULL, detail TEXT, ip TEXT, created_at DATETIME DEFAULT CURRENT_TIMESTAMP)")
	if e != nil {
		t.Fatal(e)
	}
	if e = db.InitLocalCDK(); e != nil {
		t.Fatal(e)
	}
	if e = InitOperationsProducts(); e != nil {
		t.Fatal(e)
	}
	if e = InitOperationsRecords(); e != nil {
		t.Fatal(e)
	}
	f := &localFixture{plan: "plus", quoteAmount: 2000, quoteCurrency: "USD"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Error("missing API auth")
		}
		var data any
		switch r.URL.Path {
		case "/openapi/v1/gpt-direct/card-pool/schedule":
			if r.URL.Query().Get("plan") != f.plan {
				t.Error("wrong plan used for card selection")
			}
			if r.URL.Query().Get("card_mode") != "auto_existing" {
				t.Error("unsafe card mode")
			}
			if f.poolMode == "error" {
				w.WriteHeader(503)
				return
			}
			first := gin.H{"card_id": 123, "skip": false, "available_usd": 30, "light_remain": 5}
			switch f.poolMode {
			case "low":
				first["available_usd"] = 1
			case "skip":
				first["skip"] = true
			case "missing":
				delete(first, "skip")
			case "exhausted":
				first["light_remain"] = 0
			}
			candidates := []gin.H{first, {"card_id": 456, "skip": false, "available_usd": 30, "light_remain": 5}, {"card_id": 999, "skip": false, "available_usd": 100, "light_remain": 5}}
			if f.poolMode == "none" {
				candidates = []gin.H{}
			}
			data = gin.H{"product": "gpt", "plan": f.plan, "candidates": candidates}
		case "/openapi/v1/gpt-direct/plans":
			if r.URL.Query().Get("product") != "gpt" {
				t.Error("missing product filter")
			}
			version := 2
			if f.mode == "changed-price" {
				version = 3
			}
			registry := []gin.H{{"key": f.plan, "product": "gpt", "acc_plan_key": f.plan, "purchasable": true}}
			if f.mode == "unavailable" {
				registry[0]["purchasable"] = false
			}
			fee := 100
			if f.plan != "plus" {
				fee = 15
			}
			data = gin.H{"version": version, "registry": registry, "plans": gin.H{f.plan: gin.H{"enabled": true, "serviceFeeUsdMinor": fee}}}
		case "/openapi/v1/gpt-direct/preflight":
			data = gin.H{"preflight_token": "mock-preflight-token", "preflight_expires_at": time.Now().Add(9 * time.Minute).UTC().Format(time.RFC3339), "email": "test@example.com", "currentPlan": "free", "quotes": gin.H{f.plan: gin.H{"amountMinor": f.quoteAmount, "currency": f.quoteCurrency}}}
		case "/openapi/v1/gpt-direct/orders":
			f.calls.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.request, _ = body["client_request_id"].(string)
			f.paidCards = append(f.paidCards, int64(body["card_id"].(float64)))
			f.mu.Unlock()
			if body["product"] != "gpt" || body["no_auto_card_switch"] != true || (body["card_id"] != float64(123) && body["card_id"] != float64(456)) || body["plan"] != f.plan || body["client_request_id"] != r.Header.Get("Idempotency-Key") {
				t.Error("incorrect direct order binding")
			}
			if f.mode == "uncertain" {
				w.WriteHeader(502)
				w.Write([]byte("ambiguous"))
				return
			}
			time.Sleep(10 * time.Millisecond)
			w.WriteHeader(202)
			data = gin.H{"id": 77, "status": "queued"}
		case "/openapi/v1/gpt-direct/orders/77":
			f.mu.Lock()
			request := f.request
			f.mu.Unlock()
			if f.mode == "mismatch" {
				request = "other-order"
			}
			data = gin.H{"order": gin.H{"id": 77, "client_request_id": request, "status": "completed", "completed_at": "2026-09-19T15:21:45Z"}}
		default:
			t.Errorf("unexpected upstream call %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(gin.H{"code": 0, "data": data})
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("CARD_API_BASE", upstream.URL)
	t.Setenv("CARD_API_KEY", "test-key")
	f.router = gin.New()
	f.router.POST("/issue", LocalCDKIssue)
	f.router.POST("/preview", LocalCDKPreview)
	f.router.POST("/preflight", LocalCDKPreflight)
	f.router.POST("/redeem", LocalCDKRedeem)
	f.router.POST("/result", LocalCDKResult)
	f.router.POST("/reconcile/:id", LocalCDKReconcile)
	f.router.POST("/disable/:id", LocalCDKDisable)
	return f
}
func (f *localFixture) call(path string, body any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Redemption-Device", "test-device")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, r)
	var d map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	return w.Code, d
}
func (f *localFixture) code(t *testing.T) string {
	t.Helper()
	s, d := f.call("/issue", gin.H{"count": 1, "days": 30, "request_id": "batch-0123456789012345"})
	if s != 200 {
		t.Fatalf("issue: %d %v", s, d)
	}
	return d["codes"].([]any)[0].(string)
}
func (f *localFixture) ready(t *testing.T) (string, gin.H) {
	t.Helper()
	s := localSettings{Enabled: true, CardID: 123, MinCardBalanceMinor: 2500, Currency: "USD", MaxAmountMinor: 3000, MaxFeeMinor: 100}
	b, _ := json.Marshal(s)
	_ = db.SetSetting("local_cdk_settings", string(b))
	code := f.code(t)
	status, p := f.call("/preview", gin.H{"code": code})
	if status != 200 {
		t.Fatal(p)
	}
	token := p["redemption_token"].(string)
	cred := gin.H{"mode": "session", "session": strings.Repeat("mock-only-", 8)}
	status, p = f.call("/preflight", gin.H{"redemption_token": token, "credential": cred})
	if status != 200 {
		t.Fatalf("preflight %d: %v", status, p)
	}
	return token, gin.H{"redemption_token": token, "preflight_token": p["preflight_token"], "credential": cred, "confirmed": true}
}

func TestLocalPoolSelection(t *testing.T) {
	for _, mode := range []string{"priority", "low", "skip", "missing", "exhausted", "busy", "none", "error", "uncertain"} {
		t.Run(mode, func(t *testing.T) {
			f := newLocalFixture(t)
			_, body := f.ready(t)
			cfg := readLocalSettings()
			cfg.CardID = 0
			cfg.CardIDs = []int64{123, 456}
			b, _ := json.Marshal(cfg)
			_ = db.SetSetting("local_cdk_settings", string(b))
			status, pf := f.call("/preflight", body)
			if status != 200 {
				t.Fatal(pf)
			}
			if mode == "busy" {
				_, err := db.DB.Exec("INSERT INTO local_cdks(code_hash,prefix,plan,status,expires_at,created_at,card_id) VALUES('busy','busy','plus','review',0,0,123)")
				if err != nil {
					t.Fatal(err)
				}
			} else if mode == "uncertain" {
				f.mode = "uncertain"
				f.poolMode = "low"
			} else {
				f.poolMode = mode
			}
			status, _ = f.call("/redeem", body)
			if mode == "none" || mode == "error" {
				if status < 400 || f.calls.Load() != 0 {
					t.Fatal("invalid pool paid")
				}
				return
			}
			if status != 202 || len(f.paidCards) != 1 {
				t.Fatalf("payment %d %v", status, f.paidCards)
			}
			if f.paidCards[0] != 123 && f.paidCards[0] != 456 {
				t.Fatal("card outside the randomized whitelist", f.paidCards)
			}
			var selectedCard int64
			if err := db.DB.QueryRow("SELECT card_id FROM local_card_selections").Scan(&selectedCard); err != nil || selectedCard != f.paidCards[0] {
				t.Fatal("payment-card selection was not recorded atomically", selectedCard, err)
			}
			f.call("/redeem", body)
			if f.calls.Load() != 1 {
				t.Fatal("automatic retry")
			}
		})
	}
}

func TestLocalFairRandomAvoidsLastAndBalancesUsage(t *testing.T) {
	newLocalFixture(t)
	if _, err := db.DB.Exec(`INSERT INTO local_card_selections(local_id,card_id,selected_at)
	 VALUES(1,456,50),(2,456,60),(3,123,100)`); err != nil {
		t.Fatal(err)
	}
	ids := []int64{123, 456, 789}
	if err := localFairShuffleCards(ids); err != nil {
		t.Fatal(err)
	}
	if ids[0] != 789 {
		t.Fatal("least-used non-repeating card was not first", ids)
	}
	ids = []int64{123, 456}
	if err := localFairShuffleCards(ids); err != nil {
		t.Fatal(err)
	}
	if ids[0] != 456 {
		t.Fatal("last selected card was repeated despite an alternative", ids)
	}
}

func TestLocalSelectionHistoryMigrationUsesSubmittedOrders(t *testing.T) {
	newLocalFixture(t)
	if _, err := db.DB.Exec("INSERT INTO local_cdks(id,code_hash,prefix,plan,status,expires_at,created_at,card_id,request_id,last_checked) VALUES(50,'old-selection','x','plus','consumed',0,10,456,'maple-old',20)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec("INSERT INTO automation_watch(local_id,first_seen) VALUES(50,30)"); err != nil {
		t.Fatal(err)
	}
	if err := db.InitLocalCDK(); err != nil {
		t.Fatal(err)
	}
	var cardID, selectedAt int64
	if err := db.DB.QueryRow("SELECT card_id,selected_at FROM local_card_selections WHERE local_id=50").Scan(&cardID, &selectedAt); err != nil {
		t.Fatal(err)
	}
	if cardID != 456 || selectedAt != 30 {
		t.Fatal("historical selection was not seeded correctly", cardID, selectedAt)
	}
}

func TestLegacyPaymentLimitDoesNotExcludeEligibleCard(t *testing.T) {
	f := newLocalFixture(t)
	_, body := f.ready(t)
	cfg := readLocalSettings()
	cfg.CardID = 0
	cfg.CardIDs = []int64{123, 456}
	cfg.MaxSuccessfulPaymentsPerCard = 3
	b, _ := json.Marshal(cfg)
	_ = db.SetSetting("local_cdk_settings", string(b))
	now := time.Now().Unix()
	if _, e := db.DB.Exec("UPDATE local_card_cycles SET success_limit=3,success_count=3,cycle_started_at=?,cooldown_until=?,updated_at=? WHERE card_id=123", now, now+86400, now); e != nil {
		t.Fatal(e)
	}
	preflightStatus, preflight := f.call("/preflight", body)
	if preflightStatus != 200 {
		t.Fatalf("recheck changed settings: %d %v", preflightStatus, preflight)
	}
	body["preflight_token"] = preflight["preflight_token"]
	s, _ := f.call("/redeem", body)
	if s != 202 || len(f.paidCards) != 1 || (f.paidCards[0] != 123 && f.paidCards[0] != 456) {
		t.Fatalf("eligible random card was not selected: status=%d cards=%v", s, f.paidCards)
	}
}

func TestLocalPoolNeverUsesOutsideCard(t *testing.T) {
	f := newLocalFixture(t)
	_, body := f.ready(t)
	f.poolMode = "low"
	status, _ := f.call("/redeem", body)
	if status != 409 || f.calls.Load() != 0 {
		t.Fatal("used card outside whitelist")
	}
	cfg := readLocalSettings()
	cfg.CardIDs = []int64{}
	b, _ := json.Marshal(cfg)
	_ = db.SetSetting("local_cdk_settings", string(b))
	if len(localCardIDs(cfg)) != 0 {
		t.Fatal("empty whitelist restored legacy card")
	}
	status, _ = f.call("/redeem", body)
	if status != 409 || f.calls.Load() != 0 {
		t.Fatal("removed card paid")
	}
}

func TestLocalPoolConcurrentOrdersUseDifferentCards(t *testing.T) {
	f := newLocalFixture(t)
	_, first := f.ready(t)
	cfg := readLocalSettings()
	cfg.CardID = 0
	cfg.CardIDs = []int64{123, 456}
	b, _ := json.Marshal(cfg)
	_ = db.SetSetting("local_cdk_settings", string(b))
	if s, p := f.call("/preflight", first); s != 200 {
		t.Fatal(p)
	}
	s, issued := f.call("/issue", gin.H{"count": 1, "days": 30, "request_id": "batch-second-0123456789"})
	if s != 200 {
		t.Fatal(issued)
	}
	_, preview := f.call("/preview", gin.H{"code": issued["codes"].([]any)[0]})
	second := gin.H{"redemption_token": preview["redemption_token"], "credential": first["credential"], "confirmed": true}
	s, pf := f.call("/preflight", second)
	if s != 200 {
		t.Fatal(pf)
	}
	second["preflight_token"] = pf["preflight_token"]
	var wg sync.WaitGroup
	for _, body := range []gin.H{first, second} {
		wg.Add(1)
		go func(v gin.H) { defer wg.Done(); f.call("/redeem", v) }(body)
	}
	wg.Wait()
	if f.calls.Load() != 2 || len(f.paidCards) != 2 || f.paidCards[0] == f.paidCards[1] {
		t.Fatal("card double reservation", f.paidCards)
	}
}

func TestLocalPoolSettingsValidation(t *testing.T) {
	f := newLocalFixture(t)
	f.router.POST("/settings", LocalCDKPutSettings)
	revision := func() string { raw, _ := db.GetSetting("local_cdk_settings"); return localHash(raw) }
	for _, ids := range [][]int64{{1, 1}, {0}, {-1}, make([]int64, 21)} {
		s, _ := f.call("/settings", gin.H{"card_ids": ids, "expected_revision": revision()})
		if s != 400 {
			t.Fatal("invalid whitelist saved", ids)
		}
	}
	s, _ := f.call("/settings", gin.H{"card_ids": []int64{456, 123}, "enabled": false, "expected_revision": revision()})
	if s != 200 {
		t.Fatal("valid whitelist rejected")
	}
	got := localCardIDs(readLocalSettings())
	if len(got) != 2 || got[0] != 456 {
		t.Fatal("priority not saved")
	}
	s, _ = f.call("/settings", gin.H{"card_ids": []int64{123}, "enabled": true, "currency": "USD", "max_amount_minor": 3000, "max_fee_minor": 100, "expected_revision": revision()})
	if s != 400 {
		t.Fatal("missing minimum balance accepted")
	}
}
func TestLocalIssuanceIsIndependent(t *testing.T) {
	f := newLocalFixture(t)
	t.Setenv("CARD_API_KEY", "")
	code := f.code(t)
	if !strings.HasPrefix(code, "PULS-") || len(code) != 20 || strings.Trim(strings.TrimPrefix(code, "PULS-"), "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		t.Fatal(code)
	}
	var stored string
	_ = db.DB.QueryRow("SELECT code_hash FROM local_cdks").Scan(&stored)
	if stored == code || stored != localHash(code) {
		t.Fatal("plaintext or incorrect hash")
	}
	s, _ := f.call("/issue", gin.H{"count": 1, "days": 30, "request_id": "batch-0123456789012345"})
	if s != 409 {
		t.Fatal("duplicate batch")
	}
	s, p := f.call("/preview", gin.H{"code": strings.ToLower(code)})
	if s != 200 {
		t.Fatal(p)
	}
	s, _ = f.call("/preflight", gin.H{"redemption_token": p["redemption_token"]})
	if s != 503 {
		t.Fatal("closed channel not enforced")
	}
	if f.calls.Load() != 0 {
		t.Fatal("issuance made payment")
	}
}
func TestLocalExpiryAndDisable(t *testing.T) {
	f := newLocalFixture(t)
	code := f.code(t)
	_, _ = db.DB.Exec("UPDATE local_cdks SET expires_at=1")
	s, _ := f.call("/preview", gin.H{"code": code})
	if s != 400 {
		t.Fatal("expired accepted")
	}
	_, _ = db.DB.Exec("UPDATE local_cdks SET expires_at=?,status='disabled'", time.Now().Add(time.Hour).Unix())
	s, _ = f.call("/preview", gin.H{"code": code})
	if s != 400 {
		t.Fatal("disabled accepted")
	}
}

func TestFailedLocalCodeIsPermanentAndShowsSupport(t *testing.T) {
	f := newLocalFixture(t)
	_ = f.code(t)
	_, err := db.DB.Exec("UPDATE local_cdks SET status='failed',email='buyer@example.com',upstream_id=77,message='升级未完成' WHERE id=1")
	if err != nil {
		t.Fatal(err)
	}
	r, err := loadLocal("id", "1")
	if err != nil {
		t.Fatal(err)
	}
	public := localPublicResult(r)
	if public["status"] != "failed" || public["redeem_locked"] != true || public["support_url"] != mapleSupportURL {
		t.Fatal("failed result is missing permanent lock or support", public)
	}
	if _, err = db.DB.Exec("UPDATE local_cdks SET status='unused' WHERE id=1"); err == nil {
		t.Fatal("failed code was returned to inventory")
	}
	message := localFailureAdminMessage(r, "failed_precharge", true)
	for _, want := range []string{"buyer@example.com", "PULS-", "#1", "GPT Plus", "永久锁定", "Telegram：暂未从商城同步", "发给客户的商家客服（不是客户账号）", mapleSupportURL} {
		if !strings.Contains(message, want) {
			t.Fatalf("failure alert missing %q: %s", want, message)
		}
	}
}

func TestLocalConcurrentRedeemOnlyCallsOnce(t *testing.T) {
	f := newLocalFixture(t)
	token, body := f.ready(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); f.call("/redeem", body) }()
	}
	wg.Wait()
	if f.calls.Load() != 1 {
		t.Fatalf("upstream calls: %d", f.calls.Load())
	}
	s, d := f.call("/result", gin.H{"redemption_token": token})
	if s != 200 || d["status"] != "completed" {
		t.Fatalf("result %d %v", s, d)
	}
	activated := time.Date(2026, 9, 19, 15, 21, 45, 0, time.UTC)
	if d["upgrade_type"] != "ChatGPT Plus" || d["activated_at"] != float64(activated.Unix()) || d["subscription_expires_at"] != float64(activated.AddDate(0, 1, 0).Unix()) || d["expiry_estimated"] != true {
		t.Fatalf("completed result details missing or wrong: %v", d)
	}
	f.call("/redeem", body)
	if f.calls.Load() != 1 {
		t.Fatal("replayed payment")
	}
}
func TestLocalUncertainOrderNeverRetries(t *testing.T) {
	f := newLocalFixture(t)
	f.mode = "uncertain"
	_, body := f.ready(t)
	_, d := f.call("/redeem", body)
	if d["status"] != "review" {
		t.Fatal(d)
	}
	f.call("/redeem", body)
	if f.calls.Load() != 1 {
		t.Fatal("uncertain payment repeated")
	}
	f.mode = "mismatch"
	s, _ := f.call("/reconcile/1", gin.H{"order_id": 77})
	if s != 409 {
		t.Fatal("accepted unrelated order")
	}
	f.mode = ""
	s, d = f.call("/reconcile/1", gin.H{"order_id": 77})
	if s != 200 || d["status"] != "completed" {
		t.Fatal(d)
	}
}
func TestLocalBindingAndCaps(t *testing.T) {
	f := newLocalFixture(t)
	_, body := f.ready(t)
	original := body["credential"]
	body["credential"] = gin.H{"mode": "session", "session": "different account"}
	s, _ := f.call("/redeem", body)
	if s != 409 {
		t.Fatal("credential swap allowed")
	}
	body["credential"] = original
	body["confirmed"] = false
	s, _ = f.call("/redeem", body)
	if s != 409 {
		t.Fatal("no confirmation")
	}
	body["confirmed"] = true
	settings := readLocalSettings()
	settings.MaxAmountMinor = 100
	b, _ := json.Marshal(settings)
	_ = db.SetSetting("local_cdk_settings", string(b))
	s, _ = f.call("/redeem", body)
	if s != 409 {
		t.Fatal("changed config allowed")
	}
	s, _ = f.call("/preflight", body)
	if s != 409 {
		t.Fatal("over cap quote accepted")
	}
	if f.calls.Load() != 0 {
		t.Fatal("invalid input paid")
	}
}
func TestLocalTokenDeviceAndExpiration(t *testing.T) {
	f := newLocalFixture(t)
	token, body := f.ready(t)
	_, _ = db.DB.Exec("UPDATE local_cdks SET device_hash=?", localHash("another device"))
	s, _ := f.call("/result", gin.H{"redemption_token": token})
	if s != 401 {
		t.Fatal("cross device token accepted")
	}
	_, _ = db.DB.Exec("UPDATE local_cdks SET device_hash=?,preflight_expires=1", localHash("test-device"))
	s, _ = f.call("/redeem", body)
	if s != 409 {
		t.Fatal("expired preflight accepted")
	}
	if f.calls.Load() != 0 {
		t.Fatal("expired input paid")
	}
}

func TestLocalPricingRecheckedBeforePayment(t *testing.T) {
	for _, mode := range []string{"changed-price", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			f := newLocalFixture(t)
			_, body := f.ready(t)
			f.mode = mode
			status, _ := f.call("/redeem", body)
			if status != 409 {
				t.Fatal("changed eligibility accepted")
			}
			var state string
			_ = db.DB.QueryRow("SELECT status FROM local_cdks").Scan(&state)
			if state != "unused" || f.calls.Load() != 0 {
				t.Fatal("invalid pricing reserved or paid")
			}
		})
	}
}
