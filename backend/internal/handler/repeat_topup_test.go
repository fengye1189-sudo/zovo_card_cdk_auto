package handler

import (
	"context"
	"github.com/tuzi/cdk-recharge-system/internal/db"
	"testing"
	"time"
)

func TestRepeatTopupAfterVerifiedPayment(t *testing.T) {
	for _, mode := range []string{"allowed", "busy", "budget", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			a := newAutoFixture(t)
			p := moneyPolicy()
			putAutoPolicy(t, p)
			maintainAutomationCards(context.Background())
			if a.money.Load() != 1 {
				t.Fatal("initial topup missing")
			}
			if _, err := db.DB.Exec("UPDATE automation_money SET created_at=?", time.Now().Unix()-180); err != nil {
				t.Fatal(err)
			}
			dueAgain()
			maintainAutomationCards(context.Background())
			if automationBlocked() {
				t.Fatal("first topup not confirmed")
			}
			a.balance = 5
			seedAutoOrder(t, a, 77)
			if _, err := db.DB.Exec("UPDATE local_cdks SET status='consumed' WHERE id=1"); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "busy":
				db.DB.Exec("UPDATE local_cdks SET status='review' WHERE id=1")
			case "budget":
				p.DailyBudget = 3000
				putAutoPolicy(t, p)
			case "unknown":
				db.DB.Exec("UPDATE automation_money SET state='unknown'")
			}
			dueAgain()
			maintainAutomationCards(context.Background())
			want := int32(1)
			if mode == "allowed" {
				want = 2
			}
			if a.money.Load() != want {
				t.Fatal(mode, a.money.Load(), want)
			}
			dueAgain()
			maintainAutomationCards(context.Background())
			if a.money.Load() != want {
				t.Fatal("duplicate while pending")
			}
		})
	}
}
