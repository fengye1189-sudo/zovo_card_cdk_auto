package handler

import (
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDedicatedCardNeedsConfirmedProBeforePoolAndNeverAutoTopups(t *testing.T) {
	a := newAutoFixture(t)
	if _, e := db.DB.Exec("INSERT INTO local_cdks(id,code_hash,prefix,plan,status,expires_at,created_at,card_id) VALUES(1,'pro-history','pro','pro_20x','review',0,0,123)"); e != nil {
		t.Fatal(e)
	}
	if _, e := db.DB.Exec("INSERT INTO pro_dedicated_orders(local_id,card_id,money_id,state,created_at) VALUES(1,123,'dedicated-test','submitted',0)"); e != nil {
		t.Fatal(e)
	}
	cfg := readLocalSettings()
	cfg.MinCardBalanceMinor = 100
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/", nil)
	ids, e := localAvailableCards(c, cardplatform.NewFromSettings(), cfg)
	if e != nil || len(ids) != 0 {
		t.Fatal("unconfirmed dedicated card leaked into pool")
	}
	if _, e = db.DB.Exec("UPDATE local_cdks SET status='consumed' WHERE id=1"); e != nil {
		t.Fatal(e)
	}
	if _, e = db.DB.Exec("UPDATE pro_dedicated_orders SET state='completed' WHERE local_id=1"); e != nil {
		t.Fatal(e)
	}
	cfg.CardIDs = []int64{}
	ids, e = localAvailableCards(c, cardplatform.NewFromSettings(), cfg)
	if e != nil || len(ids) != 1 || ids[0] != 123 {
		t.Fatal("confirmed dedicated card did not enter pool", ids, e)
	}
	putAutoPolicy(t, moneyPolicy())
	maintainAutomationCards(context.Background())
	if a.money.Load() != 0 {
		t.Fatal("dedicated card was automatically topped up")
	}
}

func TestProDedicatedWorkflow(t *testing.T) {
	for _, plan := range []string{"pro_5x", "pro_20x"} {
		modes := []string{"success", "budget", "shared-budget", "paused", "restart", "quote-over-limit", "upstream-disabled"}
		if plan == "pro_20x" {
			modes = append(modes, "open-unknown", "fee-change")
		}
		for _, mode := range modes {
			t.Run(plan+"/"+mode, func(t *testing.T) {
				f := newLocalFixture(t)
				initial := proDedicatedInitialMinor(plan)
				quoteAmount := int64(900000)
				if plan == "pro_5x" {
					quoteAmount = 579464
				}
				if mode == "quote-over-limit" {
					quoteAmount = settingsForLocalPlan(localSettings{}, plan).MaxAmountMinor + 1
				}
				var opens, pays atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var data any
					switch r.URL.Path {
					case "/openapi/v1/gpt-direct/plans":
						data = gin.H{"version": 2, "registry": []any{gin.H{"key": plan, "product": "gpt", "acc_plan_key": plan, "purchasable": mode != "upstream-disabled"}}, "plans": gin.H{plan: gin.H{"enabled": true, "serviceFeeUsdMinor": 15}}}
					case "/openapi/v1/gpt-direct/preflight":
						data = gin.H{"preflight_token": "mock-pf", "preflight_expires_at": time.Now().Add(9 * time.Minute).UTC().Format(time.RFC3339), "email": "mock@example.com", "currentPlan": "free", "quotes": gin.H{plan: gin.H{"amountMinor": quoteAmount, "currency": "PHP"}}}
					case "/openapi/v1/products":
						fee := 1
						if mode == "fee-change" {
							fee = 2
						}
						data = []any{gin.H{"product_code": "P5378OX", "open_fee": fee, "recharge_fee": 0, "min_amount": 20, "max_amount": 50000, "restricted_merchants": []string{}}}
					case "/openapi/v1/balance":
						data = gin.H{"spendable_balance": 1000}
					case "/openapi/v1/cards/open":
						opens.Add(1)
						var body struct {
							Product string  `json:"product_code"`
							Amount  float64 `json:"init_amount"`
						}
						json.NewDecoder(r.Body).Decode(&body)
						if body.Product != "P5378OX" || body.Amount != float64(initial)/100 || r.Header.Get("Idempotency-Key") == "" {
							t.Error("wrong dedicated opening")
						}
						if mode == "open-unknown" {
							w.WriteHeader(502)
							return
						}
						data = gin.H{"id": 789}
					case "/openapi/v1/cards":
						data = gin.H{"total": 1, "list": []any{gin.H{"id": 789, "product_code": "P5378OX", "status": "ACTIVE", "available_amount": float64(initial) / 100}}}
					case "/openapi/v1/gpt-direct/card-pool/schedule":
						data = gin.H{"product": "gpt", "plan": plan, "candidates": []any{gin.H{"card_id": 789, "skip": false, "available_usd": float64(initial) / 100, "light_remain": 5}}}
					case "/openapi/v1/gpt-direct/orders":
						pays.Add(1)
						var body map[string]any
						json.NewDecoder(r.Body).Decode(&body)
						if body["card_id"] != float64(789) || body["plan"] != plan || body["no_auto_card_switch"] != true || body["client_request_id"] != r.Header.Get("Idempotency-Key") {
							t.Error("wrong dedicated payment")
						}
						data = gin.H{"id": 77}
					default:
						t.Errorf("unexpected upstream endpoint %s", r.URL.Path)
						w.WriteHeader(404)
						return
					}
					json.NewEncoder(w).Encode(gin.H{"code": 0, "data": data})
				}))
				defer server.Close()
				t.Setenv("CARD_API_BASE", server.URL)
				cfg := localSettings{Enabled: true, ProDedicatedEnabled: true, Currency: "PHP", MaxAmountMinor: settingsForLocalPlan(localSettings{}, plan).MaxAmountMinor, MaxFeeMinor: 50}
				raw, _ := json.Marshal(cfg)
				db.SetSetting("local_cdk_settings", string(raw))
				p := moneyPolicy()
				p.Open = true
				p.DailyBudget = proDailyMaximum
				p.DailyOpen = 10
				if mode == "budget" {
					p.DailyBudget = initial
				}
				if mode == "paused" {
					p.Paused = true
				}
				putAutoPolicy(t, p)
				if plan == "pro_5x" {
					now := time.Now().Unix()
					if _, e := db.DB.Exec(`INSERT INTO automation_money(id,action,amount_minor,reserved_minor,state,created_at,result_card_id)
						VALUES('prepared-5x','pro5x_reserve_open',?,?, 'balance_verified',?,789)`, initial, initial+100, now); e != nil {
						t.Fatal(e)
					}
					if _, e := db.DB.Exec(`UPDATE pro5x_card_reserve SET card_id=789,money_id='prepared-5x',state='ready',created_at=?,updated_at=? WHERE id=1`, now, now); e != nil {
						t.Fatal(e)
					}
				}
				if mode == "shared-budget" {
					if _, e := db.DB.Exec("INSERT INTO automation_money(id,action,amount_minor,reserved_minor,state,created_at) VALUES('other-funding','topup',?,?, 'balance_verified',?)", proDailyMaximum-initial, proDailyMaximum-initial, time.Now().Unix()); e != nil {
						t.Fatal(e)
					}
				}
				status, d := f.call("/issue", gin.H{"count": 1, "days": 30, "request_id": "pro-batch-123456789", "product_id": plan})
				if status != 200 {
					t.Fatalf("issue %d %v", status, d)
				}
				status, d = f.call("/preview", gin.H{"code": d["codes"].([]any)[0]})
				if status != 200 {
					t.Fatal("preview failed")
				}
				token := d["redemption_token"]
				cred := gin.H{"mode": "session", "session": "mock-only-not-a-real-credential-12345678901234567890"}
				status, d = f.call("/preflight", gin.H{"redemption_token": token, "credential": cred})
				if mode == "quote-over-limit" || mode == "upstream-disabled" {
					if status < 400 || opens.Load() != 0 || pays.Load() != 0 {
						t.Fatalf("unsafe preflight accepted: %d %v", status, d)
					}
					return
				}
				if status != 200 {
					t.Fatalf("preflight %d %v", status, d)
				}
				if opens.Load() != 0 || pays.Load() != 0 {
					t.Fatal("preflight moved money")
				}
				if mode == "restart" {
					db.DB.Exec("UPDATE local_cdks SET status='reserved' WHERE id=1")
					db.DB.Exec("INSERT INTO automation_money(id,action,amount_minor,reserved_minor,state,created_at) VALUES('restart','pro_open',?,?, 'inflight',0)", initial, initial+115)
					db.DB.Exec("INSERT INTO pro_dedicated_orders(local_id,money_id,state,created_at) VALUES(1,'restart','opening',0)")
					recoverInterruptedProOrders()
					var state string
					db.DB.QueryRow("SELECT state FROM automation_money WHERE id='restart'").Scan(&state)
					if state != "unknown" {
						t.Fatal("restart retried or lost uncertain reservation")
					}
					return
				}
				body := gin.H{"redemption_token": token, "preflight_token": d["preflight_token"], "credential": cred, "confirmed": true}
				status, d = f.call("/redeem", body)
				if mode == "budget" || mode == "shared-budget" || mode == "fee-change" || mode == "paused" {
					if status != 409 || opens.Load() != 0 || pays.Load() != 0 {
						t.Fatalf("unsafe rejection %d", status)
					}
					return
				}
				if status != 202 {
					t.Fatalf("redeem %d %v", status, d)
				}
				var repeated sync.WaitGroup
				for i := 0; i < 5; i++ {
					repeated.Add(1)
					go func() { defer repeated.Done(); f.call("/redeem", body) }()
				}
				repeated.Wait()
				deadline := time.Now().Add(5 * time.Second)
				for {
					var n int
					if mode == "open-unknown" {
						db.DB.QueryRow("SELECT COUNT(*) FROM automation_alerts WHERE alert_key='pro:1'").Scan(&n)
					} else {
						db.DB.QueryRow("SELECT COUNT(*) FROM local_cdks WHERE upstream_id=77").Scan(&n)
					}
					if n == 1 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("worker did not finish")
					}
					time.Sleep(10 * time.Millisecond)
				}
				f.call("/redeem", body)
				f.call("/redeem", body)
				expectedOpens := int32(1)
				if plan == "pro_5x" {
					expectedOpens = 0
				}
				if opens.Load() != expectedOpens {
					t.Fatal("unexpected or repeated opening")
				}
				expected := int32(1)
				if mode == "open-unknown" {
					expected = 0
				}
				if pays.Load() != expected {
					t.Fatal("unexpected payment count")
				}
				var reserved int64
				var state string
				if plan == "pro_5x" {
					db.DB.QueryRow("SELECT COALESCE(SUM(reserved_minor),0) FROM automation_money").Scan(&reserved)
					db.DB.QueryRow("SELECT state FROM automation_money WHERE action='pro5x_pay_fee'").Scan(&state)
					if reserved != initial+115 {
						t.Fatalf("incorrect prepared-card budget reservation %d", reserved)
					}
				} else {
					db.DB.QueryRow("SELECT reserved_minor,state FROM automation_money").Scan(&reserved, &state)
					if reserved != initial+115 {
						t.Fatalf("incorrect budget reservation %d", reserved)
					}
				}
				if mode == "open-unknown" && state != "unknown" {
					t.Fatal("uncertain money not locked")
				}
				if mode == "success" {
					var card int64
					db.DB.QueryRow("SELECT card_id FROM pro_dedicated_orders").Scan(&card)
					if card != 789 || state != "balance_verified" {
						t.Fatal("dedicated binding not retained")
					}
					if plan == "pro_5x" {
						var moneyAfter int64
						if err := db.DB.QueryRow("SELECT money_after FROM automation_runtime WHERE id=1").Scan(&moneyAfter); err != nil || moneyAfter != 0 {
							t.Fatal("claimed Pro 5X reserve did not wake replacement maintenance", moneyAfter, err)
						}
					}
				}
			})
		}
	}
}
