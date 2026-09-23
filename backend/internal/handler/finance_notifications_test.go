package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func TestFinanceSyncReadOnlyAndDeduplicated(t *testing.T) {
	f := newLocalFixture(t)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" {
			t.Fatal("financial mutation")
		}
		var data any
		switch r.URL.Path {
		case "/openapi/v1/balance-logs":
			data = gin.H{"total": 1, "list": []any{gin.H{"id": 11, "card_id": 123, "ref_id": 7, "type": "card_recharge", "amount": -10, "before": 40, "after": 30, "card_number": "5378721111111234", "remark": "token: secret-do-not-store"}}}
		case "/openapi/v1/cards/123/recharges":
			data = []any{gin.H{"id": 7, "card_id": 123, "amount": 10, "fee": 0, "status": "success"}}
		case "/openapi/v1/cards/123/fund-flows":
			data = []any{gin.H{"id": 8, "amount": 10, "type": "card_recharge", "direction": "in", "card_number": "5378721111111234"}}
		case "/openapi/v1/cards/123/transactions":
			data = []any{gin.H{"auth_id": "tx-1", "type": "Settlement", "auth_amount": 15.5, "auth_currency": "USD", "settle_amount": 982.14, "settle_currency": "PHP", "merchant_name": "OPENAI", "status": "COMPLETE"}}
		default:
			t.Error("unexpected path", r.URL.Path)
		}
		json.NewEncoder(w).Encode(gin.H{"code": 0, "data": data})
	}))
	defer upstream.Close()
	t.Setenv("CARD_API_BASE", upstream.URL)
	for i := 0; i < 2; i++ {
		db.DB.Exec("UPDATE finance_sync SET next_run=0")
		syncFinance(context.Background())
	}
	var n int
	db.DB.QueryRow("SELECT COUNT(*) FROM finance_records").Scan(&n)
	if n != 3 || calls != 8 {
		t.Fatal("not deduplicated or incomplete", n, calls)
	}
	db.DB.QueryRow("SELECT COUNT(*) FROM finance_transactions").Scan(&n)
	if n != 1 {
		t.Fatal("transactions not deduplicated")
	}
	var raw, state string
	db.DB.QueryRow("SELECT payload,check_state FROM finance_records WHERE source='wallet'").Scan(&raw, &state)
	if strings.Contains(raw, "5378721111111234") || strings.Contains(raw, "secret-do-not-store") || state != "arithmetic_ok" {
		t.Fatal("unsafe ledger", state)
	}
	f.router.GET("/finance", AdminFinance)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, httptest.NewRequest("GET", "/finance", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "1234") {
		t.Fatal("ledger unavailable")
	}
	t.Setenv("CARD_API_KEY", "other-key")
	w = httptest.NewRecorder()
	f.router.ServeHTTP(w, httptest.NewRequest("GET", "/finance", nil))
	if strings.Contains(w.Body.String(), "1234") {
		t.Fatal("API scopes mixed")
	}
	if f.calls.Load() != 0 {
		t.Fatal("payment happened")
	}
}
func TestFinanceExactMoneyAndReviewSafety(t *testing.T) {
	for _, raw := range []string{"1.001", "NaN", "1/2", "1e99999999", "999999999999999999999999"} {
		if _, e := cardplatform.FinanceMinor(json.RawMessage(raw)); e == nil {
			t.Fatal("invalid money accepted", raw)
		}
	}
	n, e := cardplatform.FinanceMinor(json.RawMessage(`"-10.10"`))
	if e != nil || *n != -1010 {
		t.Fatal("decimal error")
	}
	f := newLocalFixture(t)
	f.router.POST("/review", AdminFinanceReview)
	scope := financeScope()
	amount, before, after := int64(-1000), int64(3000), int64(2000)
	entry := cardplatform.FinanceEntry{ID: 11, CardID: 123, Kind: "card_recharge", Amount: &amount, Before: &before, After: &after}
	if e := saveFinance(scope, "wallet", 0, []cardplatform.FinanceEntry{entry}); e != nil {
		t.Fatal(e)
	}
	db.DB.Exec("INSERT INTO automation_money(id,action,card_id,scope,amount_minor,reserved_minor,state,created_at) VALUES('auto-test','topup',123,?,1000,1000,'unknown',1)", scope)
	request := gin.H{"operation_id": "auto-test", "wallet_id": 11, "note": "已在上游核对编号及金额"}
	code, _ := f.call("/review", request)
	if code != 400 {
		t.Fatal("unconfirmed review")
	}
	request["confirmed"] = true
	code, _ = f.call("/review", request)
	if code != 200 {
		t.Fatal("review missing", code)
	}
	code, _ = f.call("/review", request)
	if code != 409 {
		t.Fatal("duplicate review")
	}
	if !automationBlocked() {
		t.Fatal("manual note unlocked uncertain funding")
	}
	after = 1999
	saveFinance(scope, "wallet", 0, []cardplatform.FinanceEntry{entry})
	var state string
	db.DB.QueryRow("SELECT check_state FROM finance_records").Scan(&state)
	if state != "mismatch" {
		t.Fatal("imbalance hidden")
	}
}
func TestFinancePreservesUpstreamSubcentValues(t *testing.T) {
	newLocalFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0,"data":{"total":1,"list":[{"id":1,"amount":-10,"before":40.00000000001,"after":30.00000000001,"type":"card_recharge"}]}}`)
	}))
	defer server.Close()
	cli := cardplatform.New(cardplatform.Config{SiteBase: server.URL, APIKey: "test"})
	list, _, e := cli.FinancePage(context.Background(), "wallet", 0, 1)
	if e != nil || len(list) != 1 {
		t.Fatal("subcent ledger lost", e)
	}
	if list[0].Before != nil || list[0].BeforeRaw != "40.00000000001" {
		t.Fatal("subcent value rounded")
	}
	if e = saveFinance(financeScope(), "wallet", 0, list); e != nil {
		t.Fatal(e)
	}
	var state string
	db.DB.QueryRow("SELECT check_state FROM finance_records").Scan(&state)
	if state != "precision_review" {
		t.Fatal("precision discrepancy hidden", state)
	}
}

type notificationTransport func(*http.Request) (*http.Response, error)

func (f notificationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestNotificationsDefaultsAndDelivery(t *testing.T) {
	f := newLocalFixture(t)
	f.router.POST("/notify", AdminNotificationSave)
	f.router.GET("/notify", AdminNotificationSettings)
	old := notificationHTTP
	t.Cleanup(func() { notificationHTTP = old })
	calls := 0
	notificationHTTP = &http.Client{Transport: notificationTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.telegram.org" || r.Method != "POST" {
			t.Fatal("unsafe provider URL")
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		if json.Unmarshal(body, &payload) != nil {
			t.Fatal("invalid alert payload")
		}
		for _, want := range []string{"⚠️ 枫叶兑换站 · 后台自动化异常", "首次发现：", "涉及对象：站内订单 #1", "提醒编号：problem", "当前情况：余额查询超时 <原文>", "系统处理：", "需要你处理：", "后台入口：https://cdk.maple1189ai.com/ops/automation"} {
			if !strings.Contains(payload["text"], want) {
				t.Fatal("detailed alert content missing", want, payload["text"])
			}
		}
		if strings.Contains(payload["text"], "buyer@example.com") {
			t.Fatal("customer contact leaked")
		}
		if strings.Contains(string(body), "abcdefghijklmnopqrstuvwxyz") {
			t.Fatal("credential leaked")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: http.Header{}}, nil
	})}
	autoAlert("problem", 1, "余额查询超时 <原文>")
	dispatchNotifications(context.Background())
	if calls != 0 {
		t.Fatal("notifications enabled by default")
	}
	p := notificationConfig{Enabled: true, Channel: "telegram", Recipient: "1234", Secret: "123456:abcdefghijklmnopqrstuvwxyz"}
	s, _ := f.call("/notify", gin.H{"settings": p, "version": 1})
	if s != 400 {
		t.Fatal("unconfirmed settings")
	}
	s, _ = f.call("/notify", gin.H{"settings": p, "version": 1, "confirmed": true})
	if s != 200 {
		t.Fatal("settings rejected", s)
	}
	for i := 0; i < 3; i++ {
		dispatchNotifications(context.Background())
	}
	if calls != 1 {
		t.Fatal("duplicate notification", calls)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, httptest.NewRequest("GET", "/notify", nil))
	if strings.Contains(w.Body.String(), p.Secret) {
		t.Fatal("secret reflected")
	}
	var state string
	db.DB.QueryRow("SELECT state FROM notification_outbox").Scan(&state)
	if state != "accepted" {
		t.Fatal(state)
	}
	p.Enabled = false
	p.Secret = ""
	s, _ = f.call("/notify", gin.H{"settings": p, "version": 2, "confirmed": true})
	if s != 200 {
		t.Fatal("disable failed")
	}
	autoAlert("another", 0, "another")
	dispatchNotifications(context.Background())
	if calls != 1 {
		t.Fatal("disabled channel sent")
	}
}
func TestTransientNotificationRecoversSilentlyAndPersistentOneSends(t *testing.T) {
	newLocalFixture(t)
	old := notificationHTTP
	t.Cleanup(func() { notificationHTTP = old })
	calls := 0
	notificationHTTP = &http.Client{Transport: notificationTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: http.Header{}}, nil
	})}
	p := notificationConfig{Enabled: true, Channel: "telegram", Recipient: "1234", Secret: "123456:abcdefghijklmnopqrstuvwxyz"}
	raw, _ := json.Marshal(p)
	db.DB.Exec("UPDATE notification_config SET value=?", string(raw))
	autoAlert("finance-sync", 0, "临时同步失败")
	dispatchNotifications(context.Background())
	if calls != 0 {
		t.Fatal("transient alert sent immediately")
	}
	autoResolve("finance-sync")
	dispatchNotifications(context.Background())
	if calls != 0 {
		t.Fatal("recovered alert sent")
	}
	autoAlert("finance-sync", 0, "持续同步失败")
	dispatchNotifications(context.Background())
	db.DB.Exec("UPDATE notification_alert_state SET first_seen=? WHERE alert_key='finance-sync'", time.Now().Unix()-601)
	dispatchNotifications(context.Background())
	if calls != 1 {
		t.Fatal("persistent alert not sent", calls)
	}
}
func TestNotificationUncertainAndRetryLimit(t *testing.T) {
	for _, mode := range []string{"unknown", "retry"} {
		t.Run(mode, func(t *testing.T) {
			newLocalFixture(t)
			old := notificationHTTP
			t.Cleanup(func() { notificationHTTP = old })
			calls := 0
			p := notificationConfig{Enabled: true, Channel: "email", Recipient: "to@example.com", From: "from@example.com", Secret: "re_test"}
			raw, _ := json.Marshal(p)
			db.DB.Exec("UPDATE notification_config SET value=?", string(raw))
			notificationHTTP = &http.Client{Transport: notificationTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("Idempotency-Key") == "" {
					t.Fatal("missing mail idempotency")
				}
				if mode == "unknown" {
					return nil, errors.New("secret URL")
				}
				return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader("secret body")), Header: http.Header{}}, nil
			})}
			autoAlert("x", 0, "x")
			for i := 0; i < 5; i++ {
				db.DB.Exec("UPDATE notification_outbox SET next_run=0")
				dispatchNotifications(context.Background())
			}
			expected := 1
			if mode == "retry" {
				expected = 3
			}
			if calls != expected {
				t.Fatal("wrong delivery count", calls)
			}
			var msg string
			db.DB.QueryRow("SELECT error FROM notification_outbox").Scan(&msg)
			if strings.Contains(msg, "secret") {
				t.Fatal("raw error leaked")
			}
		})
	}
}
