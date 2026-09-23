package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

type autoFixture struct {
	f                                                 *localFixture
	money, renew, deleted                             atomic.Int32
	topupAmount, topupAt                              atomic.Int64
	status, renewal, request, product, rechargeStatus string
	balance, minimum                                  float64
	empty, fail, restricted, incomplete, unavailable  bool
	willRenew                                         *bool
	extraCandidates                                   []any
}

func newAutoFixture(t *testing.T) *autoFixture {
	a := &autoFixture{f: newLocalFixture(t), status: "completed", renewal: "warning", request: "maple-merchant-12345678901234567890", balance: 5, product: "TEST", minimum: 10, rechargeStatus: "success"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Error("missing auth")
		}
		var data any
		switch r.URL.Path {
		case "/openapi/v1/gpt-direct/orders":
			if r.Method != "GET" {
				t.Fatal("automatic repayment")
			}
			data = gin.H{"total": 1, "list": []any{gin.H{"id": 77, "client_request_id": a.request}}}
		case "/openapi/v1/gpt-direct/orders/77":
			order := gin.H{"id": 77, "client_request_id": a.request, "status": a.status, "renewal_status": a.renewal, "product": "gpt"}
			if a.willRenew != nil {
				order["will_renew"] = *a.willRenew
			}
			data = gin.H{"order": order, "events": []any{}}
		case "/openapi/v1/gpt-direct/orders/77/cancel-renewal":
			a.renew.Add(1)
			if a.fail {
				w.WriteHeader(502)
				return
			}
			data = gin.H{"renewal_status": "pending"}
		case "/openapi/v1/cards":
			if r.URL.Query().Get("sync") != "1" {
				t.Error("money inventory must request fresh balances")
			}
			cards := []any{}
			if !a.empty && a.deleted.Load() == 0 {
				cards = append(cards, gin.H{"id": 123, "product_code": a.product, "status": "ACTIVE", "available_amount": a.balance, "card_number": "5378721111111234"})
			}
			total := len(cards)
			if a.incomplete {
				total++
			}
			data = gin.H{"list": cards, "total": total}
		case "/openapi/v1/gpt-direct/plans":
			data = gin.H{"version": 1, "registry": []any{gin.H{"key": "plus", "product": "gpt", "acc_plan_key": "plus", "purchasable": !a.unavailable}}, "plans": gin.H{"plus": gin.H{"enabled": true, "serviceFeeUsdMinor": 100}}}
		case "/openapi/v1/products":
			restricted := []string{}
			if a.restricted {
				restricted = []string{"CHATGPT"}
			}
			data = []any{gin.H{"product_code": a.product, "bin": "537872", "issuer": "one", "enabled": true, "open_fee": 1.5, "recharge_fee": 0.01, "min_amount": a.minimum, "max_amount": 1000, "restricted_merchants": restricted}}
		case "/openapi/v1/balance":
			data = gin.H{"spendable_balance": 100}
		case "/openapi/v1/gpt-direct/card-pool/schedule":
			candidates := []any{}
			if !a.empty {
				candidates = append(candidates, gin.H{"card_id": 123, "skip": false, "skip_reason": "", "available_usd": a.balance, "light_remain": 1})
			}
			candidates = append(candidates, a.extraCandidates...)
			data = gin.H{"product": "gpt", "plan": "plus", "candidates": candidates}
		case "/openapi/v1/cards/recharge":
			a.money.Add(1)
			if r.Method != "POST" || r.Header.Get("Idempotency-Key") == "" {
				t.Error("unsafe recharge")
			}
			if a.fail {
				w.WriteHeader(502)
				return
			}
			var body struct {
				Amount float64 `json:"amount"`
				CardID int64   `json:"card_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.CardID != 123 {
				t.Error("wrong card")
			}
			a.balance += body.Amount
			a.topupAmount.Store(int64(body.Amount*100 + 0.5))
			a.topupAt.Store(time.Now().UnixNano())
			data = nil
		case "/openapi/v1/cards/123/recharges":
			data = []any{}
			if at := a.topupAt.Load(); at > 0 {
				data = []any{gin.H{"id": 901, "card_id": 123, "status": a.rechargeStatus, "amount": float64(a.topupAmount.Load()) / 100, "created_at": time.Unix(0, at).UTC().Format(time.RFC3339Nano)}}
			}
		case "/openapi/v1/cards/open":
			a.money.Add(1)
			if r.Method != "POST" || r.Header.Get("Idempotency-Key") == "" {
				t.Error("unsafe opening")
			}
			if a.fail {
				w.WriteHeader(502)
				return
			}
			data = gin.H{"id": 789, "card_number": "5378729999997890", "cvv": "secret-cvv"}
		case "/openapi/v1/cards/123":
			if r.Method != "DELETE" || r.Header.Get("Idempotency-Key") == "" {
				t.Error("unsafe deletion")
			}
			a.deleted.Add(1)
			if a.fail {
				w.WriteHeader(502)
				return
			}
			data = gin.H{"deleted": true}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(gin.H{"code": 0, "data": data})
	}))
	t.Cleanup(server.Close)
	t.Setenv("CARD_API_BASE", server.URL)
	local := localSettings{Enabled: true, CardIDs: []int64{123}, MinCardBalanceMinor: 2500, Currency: "USD", MaxAmountMinor: 3000, MaxFeeMinor: 100}
	raw, _ := json.Marshal(local)
	db.SetSetting("local_cdk_settings", string(raw))
	return a
}
func putAutoPolicy(t *testing.T, p automationPolicy) {
	t.Helper()
	raw, _ := json.Marshal(p)
	if _, e := db.DB.Exec("UPDATE automation_policy SET value=?,version=version+1 WHERE id=1", string(raw)); e != nil {
		t.Fatal(e)
	}
}
func moneyPolicy() automationPolicy {
	return automationPolicy{Sync: true, Topup: true, DailyBudget: 5000, MaxOperation: 3000, Threshold: 1000, Target: 2500, CardCeiling: 3000, InitAmount: 2500, DailyOpen: 1, Product: "TEST", First: "Test", Last: "Fixture"}
}
func seedAutoOrder(t *testing.T, a *autoFixture, upstream int64) {
	t.Helper()
	_, e := db.DB.Exec("INSERT INTO local_cdks(id,code_hash,prefix,plan,status,expires_at,created_at,request_id,upstream_id,card_id) VALUES(1,'hash','prefix','plus','reserved',0,0,?,?,123)", a.request, upstream)
	if e != nil {
		t.Fatal(e)
	}
}
func dueAgain() {
	db.DB.Exec("UPDATE automation_watch SET next_check=0,renewal_after=0")
	db.DB.Exec("UPDATE direct_admin_actions SET attempted_at=0")
	db.DB.Exec("UPDATE automation_runtime SET money_after=0")
}
func TestAutomationRunsWithoutBrowserAndRecoversID(t *testing.T) {
	for _, up := range []int64{0, 77} {
		t.Run(string(rune(up+65)), func(t *testing.T) {
			a := newAutoFixture(t)
			seedAutoOrder(t, a, up)
			runAutomationCycle(context.Background())
			var state string
			var got int64
			db.DB.QueryRow("SELECT status,upstream_id FROM local_cdks WHERE id=1").Scan(&state, &got)
			if state != "consumed" || got != 77 {
				t.Fatalf("not recovered: %s %d", state, got)
			}
			if a.money.Load() != 0 || a.renew.Load() != 0 {
				t.Fatal("default mode mutated upstream")
			}
		})
	}
}
func TestAutomationBindingAndTerminalSafety(t *testing.T) {
	a := newAutoFixture(t)
	seedAutoOrder(t, a, 77)
	a.request = "other-order"
	runAutomationCycle(context.Background())
	var state string
	db.DB.QueryRow("SELECT status FROM local_cdks WHERE id=1").Scan(&state)
	if state != "reserved" {
		t.Fatal("wrong order consumed")
	}
	a.request = "maple-merchant-12345678901234567890"
	dueAgain()
	runAutomationCycle(context.Background())
	a.status = "queued"
	dueAgain()
	runAutomationCycle(context.Background())
	db.DB.QueryRow("SELECT status FROM local_cdks WHERE id=1").Scan(&state)
	if state != "consumed" {
		t.Fatal("terminal downgraded")
	}
}
func TestAutomationRenewalBoundedAndUnknown(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{true: "unknown", false: "limit"}[fail], func(t *testing.T) {
			a := newAutoFixture(t)
			a.fail = fail
			seedAutoOrder(t, a, 77)
			putAutoPolicy(t, automationPolicy{Sync: true, Renewal: true})
			for i := 0; i < 6; i++ {
				dueAgain()
				runAutomationCycle(context.Background())
			}
			expected := int32(3)
			if fail {
				expected = 1
			}
			if a.renew.Load() != expected {
				t.Fatal("unexpected renewal attempts", a.renew.Load())
			}
			if a.money.Load() != 0 {
				t.Fatal("unexpected money operation")
			}
		})
	}
}

func TestRenewalAlertOnlyForOutstandingCancellation(t *testing.T) {
	for _, tc := range []struct {
		status string
		alert  bool
	}{
		{status: "warning", alert: true},
		{status: "pending", alert: true},
		{status: "", alert: false},
		{status: "success", alert: false},
		{status: "not_requested", alert: false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			a := newAutoFixture(t)
			a.renewal = tc.status
			seedAutoOrder(t, a, 77)
			runAutomationCycle(context.Background())
			var unresolved int
			if err := db.DB.QueryRow("SELECT COUNT(*) FROM automation_alerts WHERE alert_key='renewal:1' AND resolved=0").Scan(&unresolved); err != nil {
				t.Fatal(err)
			}
			if (unresolved == 1) != tc.alert {
				t.Fatalf("renewal status %q alert=%v, want %v", tc.status, unresolved == 1, tc.alert)
			}
		})
	}
}

func TestRenewalMissingStatusGetsGraceAndFastRecheck(t *testing.T) {
	a := newAutoFixture(t)
	a.renewal = ""
	seedAutoOrder(t, a, 77)
	before := time.Now().Unix()
	runAutomationCycle(context.Background())
	var next int64
	var unresolved int
	if err := db.DB.QueryRow("SELECT next_check FROM automation_watch WHERE local_id=1").Scan(&next); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM automation_alerts WHERE alert_key='renewal:1' AND resolved=0").Scan(&unresolved); err != nil {
		t.Fatal(err)
	}
	if unresolved != 0 || next < before+50 || next > before+70 {
		t.Fatalf("grace alert=%d next=%d before=%d", unresolved, next, before)
	}
	if _, err := db.DB.Exec("UPDATE local_cdks SET activated_at=? WHERE id=1", before-301); err != nil {
		t.Fatal(err)
	}
	dueAgain()
	runAutomationCycle(context.Background())
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM automation_alerts WHERE alert_key='renewal:1' AND resolved=0").Scan(&unresolved); err != nil {
		t.Fatal(err)
	}
	if unresolved != 1 {
		t.Fatal("missing renewal status did not alert after grace period")
	}
}

func TestRenewalWillRenewFalseIsConfirmed(t *testing.T) {
	a := newAutoFixture(t)
	a.renewal = ""
	value := false
	a.willRenew = &value
	seedAutoOrder(t, a, 77)
	runAutomationCycle(context.Background())
	var unresolved int
	var next int64
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM automation_alerts WHERE alert_key='renewal:1' AND resolved=0").Scan(&unresolved); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow("SELECT next_check FROM automation_watch WHERE local_id=1").Scan(&next); err != nil {
		t.Fatal(err)
	}
	if unresolved != 0 || next < time.Now().Unix()+86390 {
		t.Fatalf("confirmed cancellation alert=%d next=%d", unresolved, next)
	}
}

func TestAutomationWaitsForCancellationVerificationBeforeAlerting(t *testing.T) {
	a := newAutoFixture(t)
	seedAutoOrder(t, a, 77)
	putAutoPolicy(t, automationPolicy{Sync: true, Renewal: true})
	for attempt := 1; attempt <= 3; attempt++ {
		dueAgain()
		runAutomationCycle(context.Background())
		var unresolved int
		if err := db.DB.QueryRow("SELECT COUNT(*) FROM automation_alerts WHERE alert_key='renewal:1' AND resolved=0").Scan(&unresolved); err != nil {
			t.Fatal(err)
		}
		wantAlert := attempt == 3
		if (unresolved == 1) != wantAlert {
			t.Fatalf("attempt %d alert=%v, want %v", attempt, unresolved == 1, wantAlert)
		}
	}
}
func TestAutomationPauseAndSettings(t *testing.T) {
	a := newAutoFixture(t)
	seedAutoOrder(t, a, 77)
	putAutoPolicy(t, automationPolicy{Sync: true, Paused: true, Renewal: true})
	runAutomationCycle(context.Background())
	if a.renew.Load() != 0 || !automationBlocked() {
		t.Fatal("pause ignored")
	}
	var state string
	db.DB.QueryRow("SELECT status FROM local_cdks WHERE id=1").Scan(&state)
	if state != "consumed" {
		t.Fatal("pause stopped readonly reconciliation")
	}
	if validateAutomation(automationPolicy{Topup: true}) {
		t.Fatal("unbudgeted funding enabled")
	}
	if validateAutomation(automationPolicy{Sync: false, Renewal: true}) {
		t.Fatal("actions without synchronization")
	}
}
func TestAutomationTopupAndPendingVerification(t *testing.T) {
	a := newAutoFixture(t)
	putAutoPolicy(t, moneyPolicy())
	maintainAutomationCards(context.Background())
	if a.money.Load() != 1 || !automationBlocked() {
		t.Fatal("topup not reserved")
	}
	dueAgain()
	maintainAutomationCards(context.Background())
	if a.money.Load() != 1 {
		t.Fatal("pending topup repeated")
	}
	db.DB.Exec("UPDATE automation_money SET created_at=?", time.Now().Unix()-180)
	dueAgain()
	maintainAutomationCards(context.Background())
	var state string
	db.DB.QueryRow("SELECT state FROM automation_money").Scan(&state)
	if state != "balance_verified" || automationBlocked() {
		t.Fatal("balance not verified", state)
	}
	if a.money.Load() != 1 {
		t.Fatal("extra funding")
	}
}

func TestAutomationTopupLedgerConfirmsAfterCardLeavesInventory(t *testing.T) {
	a := newAutoFixture(t)
	p := moneyPolicy()
	putAutoPolicy(t, p)
	maintainAutomationCards(context.Background())
	if a.money.Load() != 1 || !automationBlocked() {
		t.Fatal("topup not pending")
	}
	// Simulate a card being spent or archived before the next inventory scan.
	// The exact successful recharge ledger entry must still confirm the request.
	if _, err := db.DB.Exec("UPDATE automation_money SET created_at=?", time.Now().Unix()-180); err != nil {
		t.Fatal(err)
	}
	cfg := cardplatform.LoadConfig()
	verifyMoneyOperationsForScope(nil, p, localHash(cfg.SiteBase+"|"+cfg.APIKey))
	var state string
	if err := db.DB.QueryRow("SELECT state FROM automation_money").Scan(&state); err != nil || state != "balance_verified" || automationBlocked() {
		t.Fatal("ledger did not confirm topup", state, err)
	}
	var evidence int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM automation_money_evidence WHERE source='card_recharge'").Scan(&evidence); err != nil || evidence != 1 {
		t.Fatal("unique ledger evidence missing", evidence, err)
	}
}

func TestAutomationTopupLedgerRejectsUntrustedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		adjust func(*autoFixture)
	}{
		{name: "wrong amount", adjust: func(a *autoFixture) { a.topupAmount.Store(999) }},
		{name: "failed status", adjust: func(a *autoFixture) { a.rechargeStatus = "failed" }},
		{name: "outside operation window", adjust: func(a *autoFixture) { a.topupAt.Store(time.Now().Add(-time.Hour).UnixNano()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAutoFixture(t)
			p := moneyPolicy()
			putAutoPolicy(t, p)
			maintainAutomationCards(context.Background())
			tc.adjust(a)
			if _, err := db.DB.Exec("UPDATE automation_money SET created_at=?", time.Now().Unix()-180); err != nil {
				t.Fatal(err)
			}
			cfg := cardplatform.LoadConfig()
			verifyMoneyOperationsForScope(nil, p, localHash(cfg.SiteBase+"|"+cfg.APIKey))
			if !automationBlocked() {
				t.Fatal("untrusted ledger entry released the lock")
			}
			var evidence int
			if err := db.DB.QueryRow("SELECT COUNT(*) FROM automation_money_evidence").Scan(&evidence); err != nil || evidence != 0 {
				t.Fatal("untrusted evidence was recorded", evidence, err)
			}
		})
	}
}
func TestAutomationMoneyLimitsAndUnknown(t *testing.T) {
	for _, mode := range []string{"budget", "per_operation", "restricted", "busy", "unknown", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			a := newAutoFixture(t)
			p := moneyPolicy()
			switch mode {
			case "budget":
				p.DailyBudget = 100
			case "per_operation":
				p.MaxOperation = 100
			case "restricted":
				a.restricted = true
			case "busy":
				seedAutoOrder(t, a, 77)
			case "unknown":
				a.fail = true
			case "disabled":
				p.Topup = false
			}
			putAutoPolicy(t, p)
			maintainAutomationCards(context.Background())
			dueAgain()
			maintainAutomationCards(context.Background())
			want := int32(0)
			if mode == "unknown" {
				want = 1
				if !automationBlocked() {
					t.Fatal("unknown did not block")
				}
			}
			if a.money.Load() != want {
				t.Fatal("unexpected funding", mode, a.money.Load())
			}
		})
	}
}
func TestAutomationOpenBudgetAndEnrollment(t *testing.T) {
	a := newAutoFixture(t)
	a.empty = true
	p := moneyPolicy()
	p.Topup = false
	p.Open = true
	p.Enroll = true
	putAutoPolicy(t, p)
	maintainAutomationCards(context.Background())
	if a.money.Load() != 1 {
		t.Fatal("opening missing")
	}
	var id int64
	db.DB.QueryRow("SELECT result_card_id FROM automation_money").Scan(&id)
	if id != 789 {
		t.Fatal("id missing")
	}
	db.DB.Exec("UPDATE automation_money SET created_at=?", time.Now().Unix()-180)
	balance := 25.0
	verifyMoneyOperations([]cardplatform.CardChoice{{ID: 789, Status: "ACTIVE", Balance: &balance}}, p)
	ids := localCardIDs(readLocalSettings())
	if len(ids) != 2 || ids[1] != 789 {
		t.Fatal("enrollment missing")
	}
	dueAgain()
	maintainAutomationCards(context.Background())
	if a.money.Load() != 1 {
		t.Fatal("daily opening limit ignored")
	}
}

func TestReconcileCreatedCardEnrollmentHealsOrdinaryAndCompletedPro5x(t *testing.T) {
	newLocalFixture(t)
	raw, _ := json.Marshal(localSettings{Enabled: true, CardIDs: []int64{123}})
	if err := db.SetSetting("local_cdk_settings", string(raw)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.DB.Exec(`INSERT INTO automation_money
		(id,action,card_id,result_card_id,amount_minor,reserved_minor,before_minor,scope,state,created_at)
		VALUES('opened-card','open',0,456,2100,2200,0,'scope','balance_verified',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec("INSERT INTO local_cdks(id,code_hash,prefix,plan,status,expires_at,created_at,card_id) VALUES(1,'pro5x-enroll','PRO5X-TEST','pro_5x','consumed',?,?,789)", now+86400, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec("INSERT INTO pro_dedicated_orders(local_id,card_id,money_id,state,created_at) VALUES(1,789,'pro5x-enroll-money','completed',?)", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec("INSERT INTO local_card_cycles(card_id,card_kind,success_limit,success_count,cycle_started_at,cooldown_until,updated_at) VALUES(789,'ordinary',5,0,?,0,?)", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec("INSERT OR IGNORE INTO automation_card_lifecycle(card_id,created_at,updated_at) VALUES(789,?,?)", now, now); err != nil {
		t.Fatal(err)
	}
	balance := 25.0
	reconcileCreatedCardEnrollment([]cardplatform.CardChoice{
		{ID: 456, Status: "ACTIVE", Balance: &balance},
		{ID: 789, Status: "ACTIVE", Balance: &balance},
	}, automationPolicy{Enroll: true})
	ids := localCardIDs(readLocalSettings())
	if len(ids) != 3 || ids[0] != 123 || ids[1] != 456 || ids[2] != 789 {
		t.Fatal("eligible site-created cards were not healed into the list", ids)
	}
	var actions int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM admin_audit_logs WHERE action='enroll_opened_card'").Scan(&actions); err != nil || actions != 2 {
		t.Fatal("enrollment audit missing", actions, err)
	}
}

func TestAutomationOpenThresholdKeepsThreeUsableCards(t *testing.T) {
	for ready, want := range map[int]bool{0: true, 1: true, 2: true, 3: false, 4: false} {
		if got := shouldOpenAutomationCard(ready); got != want {
			t.Fatalf("ready=%d: got %t want %t", ready, got, want)
		}
	}
}
func TestAutomationMoneyScopeChange(t *testing.T) {
	newAutoFixture(t)
	putAutoPolicy(t, moneyPolicy())
	maintainAutomationCards(context.Background())
	db.DB.Exec("UPDATE automation_money SET created_at=?", time.Now().Unix()-180)
	t.Setenv("CARD_API_KEY", "rotated")
	balance := 25.0
	verifyMoneyOperations([]cardplatform.CardChoice{{ID: 123, Status: "ACTIVE", Balance: &balance}}, moneyPolicy())
	if !automationBlocked() {
		t.Fatal("other API identity released lock")
	}
}
func TestAutomationConcurrentMoney(t *testing.T) {
	a := newAutoFixture(t)
	putAutoPolicy(t, moneyPolicy())
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); maintainAutomationCards(context.Background()) }()
	}
	wg.Wait()
	if a.money.Load() != 1 {
		t.Fatal("concurrent funding duplicated")
	}
}
func TestAutomationPublicRedeemPaused(t *testing.T) {
	f := newLocalFixture(t)
	_, body := f.ready(t)
	putAutoPolicy(t, automationPolicy{Sync: true, Paused: true})
	s, _ := f.call("/redeem", body)
	if s != 409 || f.calls.Load() != 0 {
		t.Fatal("paused payment escaped")
	}
}
func TestAutomationPolicyConfirmationAndStaleVersion(t *testing.T) {
	f := newLocalFixture(t)
	f.router.POST("/auto-save", AdminAutomationSave)
	s, _ := f.call("/auto-save", gin.H{"settings": gin.H{"sync_enabled": true}, "version": 1})
	if s != 400 {
		t.Fatal("confirmation omitted")
	}
	s, _ = f.call("/auto-save", gin.H{"settings": gin.H{"sync_enabled": true}, "version": 1, "confirmed": true})
	if s != 200 {
		t.Fatal("save failed")
	}
	s, _ = f.call("/auto-save", gin.H{"settings": gin.H{"sync_enabled": true}, "version": 1, "confirmed": true})
	if s != 409 {
		t.Fatal("stale save allowed")
	}
}
func TestAutomationWebhookIsOnlyWakeup(t *testing.T) {
	a := newAutoFixture(t)
	seedAutoOrder(t, a, 77)
	db.DB.Exec("INSERT INTO automation_watch(local_id,next_check) VALUES(1,?)", time.Now().Unix()+3600)
	notifyAutomationWebhook(map[string]interface{}{"client_request_id": a.request, "order_id": float64(77), "status": "completed"})
	var state string
	db.DB.QueryRow("SELECT status FROM local_cdks WHERE id=1").Scan(&state)
	if state != "reserved" {
		t.Fatal("webhook changed authoritative result")
	}
}
func TestAutomationFeeRounding(t *testing.T) {
	min, max, fee := 10.005, 100.0, 1.555
	restricted := []string{}
	p := cardplatform.AutomationProduct{Min: &min, Max: &max, OpenFee: &fee, Restricted: &restricted}
	if _, ok := productCost(p, 1000, true); ok {
		t.Fatal("below minimum")
	}
	cost, ok := productCost(p, 2000, true)
	if !ok || cost != 2156 {
		t.Fatal("fee underreserved", cost)
	}
}
func TestAutomationInventoryScopeMustMatch(t *testing.T) {
	a := newAutoFixture(t)
	putAutoPolicy(t, moneyPolicy())
	maintainAutomationCards(context.Background())
	db.DB.Exec("UPDATE automation_money SET created_at=?", time.Now().Unix()-180)
	balance := 25.0
	verifyMoneyOperationsForScope([]cardplatform.CardChoice{{ID: 123, Status: "ACTIVE", Balance: &balance}}, moneyPolicy(), "different-inventory-source")
	if !automationBlocked() || a.money.Load() != 1 {
		t.Fatal("unrelated inventory unlocked operation")
	}
}
func TestAutomationDoesNotFundIncompleteInventoryOrClosedPlan(t *testing.T) {
	for _, mode := range []string{"inventory", "plan"} {
		t.Run(mode, func(t *testing.T) {
			a := newAutoFixture(t)
			a.empty = true
			p := moneyPolicy()
			p.Topup = false
			p.Open = true
			putAutoPolicy(t, p)
			if mode == "inventory" {
				a.incomplete = true
			} else {
				a.unavailable = true
			}
			maintainAutomationCards(context.Background())
			if a.money.Load() != 0 {
				t.Fatal("unsafe opening")
			}
		})
	}
}
