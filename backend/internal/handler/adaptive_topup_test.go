package handler

import (
	"context"
	"github.com/tuzi/cdk-recharge-system/internal/db"
	"testing"
	"time"
)

func TestAdaptiveTopupBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name          string
		balance       float64
		ceiling, want int64
	}{
		{"empty", 0, 2600, 1800}, {"after_upgrade", 2.13, 2600, 1587},
		{"eight", 8, 2600, 1000}, {"near_threshold", 15.86, 2600, 1000},
		{"at_threshold", 15.87, 2600, 0}, {"funded", 18, 2600, 0},
		{"actual_ceiling", 15.86, 2150, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAutoFixture(t)
			a.balance = tc.balance
			p := moneyPolicy()
			p.Threshold = 1587
			p.Target = 1800
			p.CardCeiling = tc.ceiling
			p.MaxOperation = 2200
			putAutoPolicy(t, p)
			if err := db.SetSetting("local_cdk_settings", `{"enabled":true,"card_ids":[123],"min_card_balance_minor":1600,"max_fee_minor":100}`); err != nil {
				t.Fatal(err)
			}
			maintainAutomationCards(context.Background())
			if tc.want == 0 {
				if a.money.Load() != 0 {
					t.Fatal("unexpected funding")
				}
				return
			}
			var amount int64
			if err := db.DB.QueryRow("SELECT amount_minor FROM automation_money WHERE action='topup'").Scan(&amount); err != nil || amount != tc.want {
				t.Fatal(amount, tc.want, err)
			}
			dueAgain()
			maintainAutomationCards(context.Background())
			if a.money.Load() != 1 {
				t.Fatal("duplicate pending funding")
			}
		})
	}
}

func TestAdaptiveTopupIgnoresRetiredLocalUseCaps(t *testing.T) {
	for limit := 1; limit <= 5; limit++ {
		for _, exhausted := range []bool{false, true} {
			t.Run(string(rune('0'+limit))+map[bool]string{false: "remaining", true: "exhausted"}[exhausted], func(t *testing.T) {
				a := newAutoFixture(t)
				a.balance = 2.13
				p := moneyPolicy()
				p.Threshold = 1587
				p.Target = 1800
				p.CardCeiling = 2600
				putAutoPolicy(t, p)
				if err := db.SetSetting("local_cdk_settings", `{"enabled":true,"card_ids":[123],"min_card_balance_minor":1600,"max_fee_minor":100}`); err != nil {
					t.Fatal(err)
				}
				if _, err := localCardHasPaymentCapacity(123, 0); err != nil {
					t.Fatal(err)
				}
				count, until := limit-1, int64(0)
				if exhausted {
					count = limit
					until = time.Now().Unix() + 86400
				}
				if _, err := db.DB.Exec("UPDATE local_card_cycles SET success_limit=?,success_count=?,cooldown_until=? WHERE card_id=123", limit, count, until); err != nil {
					t.Fatal(err)
				}
				maintainAutomationCards(context.Background())
				want := int32(1)
				if a.money.Load() != want {
					t.Fatal("legacy capacity unexpectedly blocked funding", a.money.Load(), want)
				}
			})
		}
	}
}
