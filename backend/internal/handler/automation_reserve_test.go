package handler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func TestAutomationKeepsThreeUsableCards(t *testing.T) {
	for _, mode := range []string{"zero", "one", "two", "duplicate", "cooldown", "unselected", "insufficient", "disabled", "missing_product"} {
		t.Run(mode, func(t *testing.T) {
			a := newAutoFixture(t)
			a.balance = 25
			p := moneyPolicy()
			p.Topup, p.Open = false, true
			p.DailyOpen = 10
			p.DailyBudget = 50000
			cfg := readLocalSettings()
			if mode == "zero" {
				a.empty = true
			}
			if mode == "disabled" {
				p.Open = false
			}
			if mode == "missing_product" {
				p.Product = "MISSING"
			}
			switch mode {
			case "two", "duplicate", "cooldown", "unselected", "insufficient":
				id := int64(456)
				balance := 25.0
				if mode == "duplicate" {
					id = 123
				}
				if mode == "insufficient" {
					balance = 5
				}
				a.extraCandidates = []any{gin.H{"card_id": id, "skip": false, "skip_reason": "", "available_usd": balance, "light_remain": 1}}
				if mode != "unselected" {
					cfg.CardIDs = append(cfg.CardIDs, id)
				}
				if mode == "cooldown" {
					if _, err := db.DB.Exec("INSERT INTO local_card_cycles VALUES(456,'ordinary',3,3,?,?,?)", time.Now().Unix(), time.Now().Unix()+86400, time.Now().Unix()); err != nil {
						t.Fatal(err)
					}
				}
			}
			raw, _ := json.Marshal(cfg)
			if err := db.SetSetting("local_cdk_settings", string(raw)); err != nil {
				t.Fatal(err)
			}
			putAutoPolicy(t, p)
			maintainAutomationCards(context.Background())
			want := int32(1)
			// The upstream client rejects duplicate candidate IDs rather than guessing.
			if mode == "disabled" || mode == "duplicate" || mode == "missing_product" {
				want = 0
			}
			if mode == "missing_product" {
				var n int
				if err := db.DB.QueryRow("SELECT COUNT(*) FROM automation_alerts WHERE alert_key='money' AND resolved=0").Scan(&n); err != nil || n != 1 {
					t.Fatal("missing product must be visible", err)
				}
			}
			if a.money.Load() != want {
				t.Fatalf("opening count: got %d want %d", a.money.Load(), want)
			}
			if want == 1 {
				var product, selectionMode string
				if err := db.DB.QueryRow("SELECT product_code,selection_mode FROM automation_product_selections").Scan(&product, &selectionMode); err != nil || product != "TEST" || selectionMode != "fixed" {
					t.Fatal("opening product choice was not recorded atomically", product, selectionMode, err)
				}
				var cardType string
				if err := db.DB.QueryRow("SELECT card_type FROM automation_product_catalog WHERE product_code='TEST'").Scan(&cardType); err != nil || cardType != "channel1" {
					t.Fatal("upstream card type was not cataloged", cardType, err)
				}
			}
			dueAgain()
			maintainAutomationCards(context.Background())
			if a.money.Load() != want {
				t.Fatal("repeated opening while awaiting confirmation")
			}
		})
	}
}

func TestAutomationContinuesUntilThreeUsableCards(t *testing.T) {
	a := newAutoFixture(t)
	a.balance = 25
	p := moneyPolicy()
	p.Topup, p.Open, p.Enroll = false, true, true
	p.DailyOpen, p.DailyBudget = 10, 50000
	putAutoPolicy(t, p)
	maintainAutomationCards(context.Background())
	if a.money.Load() != 1 {
		t.Fatal("reserve not opened")
	}
	if _, err := db.DB.Exec("UPDATE automation_money SET created_at=?", time.Now().Unix()-180); err != nil {
		t.Fatal(err)
	}
	balance := 25.0
	verifyMoneyOperations([]cardplatform.CardChoice{{ID: 789, Status: "ACTIVE", Balance: &balance}}, p)
	a.extraCandidates = []any{gin.H{"card_id": 789, "skip": false, "skip_reason": "", "available_usd": 25, "light_remain": 5}}
	dueAgain()
	maintainAutomationCards(context.Background())
	if a.money.Load() != 2 {
		t.Fatal("second reserve was not opened when only two cards were usable")
	}
	dueAgain()
	maintainAutomationCards(context.Background())
	if a.money.Load() != 2 {
		t.Fatal("opening repeated while the second reserve awaited confirmation")
	}
}
