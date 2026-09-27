package handler

import (
	"testing"

	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func TestCardPlatformCostEventsDoNotDoubleCountFee(t *testing.T) {
	_ = newLocalFixture(t)
	if err := InitCardPlatformInsights(); err != nil {
		t.Fatal(err)
	}
	recordCardPlatformCostEvent(map[string]interface{}{
		"balance_log_id": float64(81), "type": "decline_fee", "amount": -0.30,
		"direction": "debit", "currency": "USD", "card_id": float64(42), "ref_id": float64(9),
	}, "balance_change")
	recordCardPlatformCostEvent(map[string]interface{}{
		"auth_id": "AUTH-1", "fee_type": "decline_fee", "amount": 0.30,
		"direction": "debit", "currency": "USD", "occurred_at": "2026-09-27 10:00:00",
	}, "card_fee")
	// At-least-once webhook replay must update rather than duplicate.
	recordCardPlatformCostEvent(map[string]interface{}{
		"balance_log_id": float64(81), "type": "decline_fee", "amount": -0.30,
		"direction": "debit", "currency": "USD", "card_id": float64(42), "ref_id": float64(9),
	}, "balance_change")
	var rows int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM cardplatform_cost_events").Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("unexpected event count %d: %v", rows, err)
	}
	var ledger, attribution int64
	if err := db.DB.QueryRow("SELECT COALESCE(SUM(ledger_amount_minor),0),COALESCE(SUM(attribution_amount_minor),0) FROM cardplatform_cost_events").Scan(&ledger, &attribution); err != nil {
		t.Fatal(err)
	}
	if ledger != -30 || attribution != 30 {
		t.Fatalf("fee views were not separated: ledger=%d attribution=%d", ledger, attribution)
	}
}

func TestWebhookIdempotencyUsesStableFinancialIdentifiers(t *testing.T) {
	balance := webhookIdemKey(map[string]interface{}{"balance_log_id": float64(7)}, "balance_change")
	if balance != "balance_change|7" {
		t.Fatal(balance)
	}
	progress := webhookIdemKey(map[string]interface{}{"event_id": "gpt_direct.progress:88:3"}, "gpt_direct.progress")
	if progress != "gpt_direct.progress|gpt_direct.progress:88:3" {
		t.Fatal(progress)
	}
}
