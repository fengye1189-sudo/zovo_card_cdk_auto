package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func TestAutomationKeepsExactlyOneVerifiedPro5xReserve(t *testing.T) {
	newLocalFixture(t)
	var opens atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/openapi/v1/cards":
			cards := []any{}
			if opens.Load() > 0 {
				cards = append(cards, gin.H{"id": 789, "product_code": pro5xReserveProduct, "status": "ACTIVE", "available_amount": 100.0})
			}
			data = gin.H{"list": cards, "total": len(cards)}
		case "/openapi/v1/products":
			data = []any{gin.H{"product_code": pro5xReserveProduct, "bin": "537872", "issuer": "one", "enabled": true, "open_fee": 1.0, "recharge_fee": 0.0, "min_amount": 20.0, "max_amount": 500.0, "restricted_merchants": []string{}}}
		case "/openapi/v1/balance":
			data = gin.H{"spendable_balance": 1000.0}
		case "/openapi/v1/cards/open":
			opens.Add(1)
			var body struct {
				Product string  `json:"product_code"`
				Amount  float64 `json:"init_amount"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Product != pro5xReserveProduct || body.Amount != 100 || r.Header.Get("Idempotency-Key") == "" {
				t.Errorf("wrong reserve opening: %+v", body)
			}
			data = gin.H{"id": 789}
		default:
			t.Fatalf("unexpected upstream endpoint %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(gin.H{"code": 0, "data": data})
	}))
	defer server.Close()
	t.Setenv("CARD_API_BASE", server.URL)

	settings := localSettings{Enabled: true, ProDedicatedEnabled: true}
	raw, _ := json.Marshal(settings)
	db.SetSetting("local_cdk_settings", string(raw))
	policy := automationPolicy{Sync: true, Open: true, DailyBudget: proDailyMaximum, MaxOperation: 2200, WalletFloor: 3000, CardCeiling: 2600, InitAmount: 2100, DailyOpen: 10, First: "Maple", Last: "Reserve", Product: pro5xReserveProduct}
	putAutoPolicy(t, policy)

	maintainAutomationCards(context.Background())
	if opens.Load() != 1 {
		t.Fatalf("expected one reserve opening, got %d", opens.Load())
	}
	var state, moneyState string
	var cardID int64
	if err := db.DB.QueryRow("SELECT state,card_id FROM pro5x_card_reserve WHERE id=1").Scan(&state, &cardID); err != nil || state != "funding" || cardID != 789 {
		t.Fatal("reserve did not enter funding", state, cardID, err)
	}
	if err := db.DB.QueryRow("SELECT state FROM automation_money WHERE action='pro5x_reserve_open'").Scan(&moneyState); err != nil || moneyState != "pending" {
		t.Fatal("reserve money was not pending", moneyState, err)
	}

	_, _ = db.DB.Exec("UPDATE automation_money SET created_at=? WHERE action='pro5x_reserve_open'", time.Now().Unix()-121)
	_, _ = db.DB.Exec("UPDATE automation_runtime SET money_after=0 WHERE id=1")
	maintainAutomationCards(context.Background())
	if err := db.DB.QueryRow("SELECT state FROM pro5x_card_reserve WHERE id=1").Scan(&state); err != nil || state != "ready" {
		t.Fatal("verified reserve did not become ready", state, err)
	}
	var moneyAfter int64
	if err := db.DB.QueryRow("SELECT money_after FROM automation_runtime WHERE id=1").Scan(&moneyAfter); err != nil || moneyAfter != 0 {
		t.Fatal("ready reserve did not wake released-card maintenance", moneyAfter, err)
	}
	maintainAutomationCards(context.Background())
	if opens.Load() != 1 {
		t.Fatalf("ready reserve triggered duplicate opening: %d", opens.Load())
	}
}

func TestInterruptedPro5xReserveNeverReopensAutomatically(t *testing.T) {
	newLocalFixture(t)
	now := time.Now().Unix()
	if _, err := db.DB.Exec(`INSERT INTO automation_money(id,action,amount_minor,reserved_minor,state,created_at)
		VALUES('interrupted-reserve','pro5x_reserve_open',10000,10100,'inflight',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`UPDATE pro5x_card_reserve SET money_id='interrupted-reserve',state='opening',created_at=?,updated_at=? WHERE id=1`, now, now); err != nil {
		t.Fatal(err)
	}
	recoverInterruptedPro5xReserve()
	var reserveState, moneyState string
	if err := db.DB.QueryRow("SELECT state FROM pro5x_card_reserve WHERE id=1").Scan(&reserveState); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow("SELECT state FROM automation_money WHERE id='interrupted-reserve'").Scan(&moneyState); err != nil {
		t.Fatal(err)
	}
	if reserveState != "review" || moneyState != "unknown" {
		t.Fatal("interrupted reserve was not safely locked", reserveState, moneyState)
	}
}

func TestReviewedPro5xReserveRecoversFromVerifiedBalanceEvidence(t *testing.T) {
	newLocalFixture(t)
	now := time.Now().Unix()
	if _, err := db.DB.Exec(`INSERT INTO automation_money(id,action,card_id,amount_minor,reserved_minor,state,created_at,result_card_id)
		VALUES('verified-reserve','pro5x_reserve_open',0,10000,10100,'balance_verified',?,379135)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`UPDATE pro5x_card_reserve SET card_id=379135,money_id='verified-reserve',state='review',created_at=?,updated_at=? WHERE id=1`, now, now); err != nil {
		t.Fatal(err)
	}
	autoAlert("pro5x_reserve", 0, "Pro 5X 专卡准备结果需要核对；系统不会重复开卡。")
	balance := 100.0
	reconcilePro5xReserve([]cardplatform.CardChoice{{ID: 379135, Status: "ACTIVE", Balance: &balance}})

	var state string
	if err := db.DB.QueryRow("SELECT state FROM pro5x_card_reserve WHERE id=1").Scan(&state); err != nil || state != "ready" {
		t.Fatal("verified reserve did not recover", state, err)
	}
	var resolved int
	if err := db.DB.QueryRow("SELECT resolved FROM automation_alerts WHERE alert_key='pro5x_reserve'").Scan(&resolved); err != nil || resolved != 1 {
		t.Fatal("verified reserve alert was not resolved", resolved, err)
	}
}
