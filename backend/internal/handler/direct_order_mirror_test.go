package handler

import (
	"testing"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func TestDirectOrderMirrorIsIdempotentAndKeepsUsableEmail(t *testing.T) {
	newLocalFixture(t)
	completed := time.Date(2026, 9, 28, 12, 30, 0, 0, time.UTC)
	order := map[string]any{
		"id": float64(304414), "client_request_id": "merchant-304414",
		"account_email": "Customer@Example.com", "product": "gpt", "plan": "plus",
		"status": "completed", "card_id": float64(88), "card_last_four": "7996",
		"created_at":   completed.Add(-30 * time.Second).Format(time.RFC3339),
		"completed_at": completed.Format(time.RFC3339),
	}
	if err := mirrorDirectOrder(order, "zovo_api", completed.Unix()); err != nil {
		t.Fatal(err)
	}
	// A later masked webhook must not erase the usable account identity.
	if err := mirrorDirectOrder(map[string]any{
		"id": float64(304414), "account_email": "cu***@example.com", "status": "completed",
	}, "zovo_webhook", completed.Add(time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	var email, status, source string
	var activated, expires, count int64
	if err := db.DB.QueryRow(`SELECT account_email,status,source,activated_at,subscription_expires_at
	 FROM zovo_direct_order_mirror WHERE upstream_id=304414`).Scan(&email, &status, &source, &activated, &expires); err != nil {
		t.Fatal(err)
	}
	if email != "customer@example.com" || status != "completed" || source != "zovo_api" || activated != completed.Unix() || expires != completed.AddDate(0, 1, 0).Unix() {
		t.Fatalf("unexpected mirror: %q %q %q %d %d", email, status, source, activated, expires)
	}
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM zovo_direct_order_mirror WHERE upstream_id=304414").Scan(&count); err != nil || count != 1 {
		t.Fatalf("mirror was not idempotent: count=%d err=%v", count, err)
	}
}

func TestDirectOrderMirrorRejectsMaskedCustomerIdentity(t *testing.T) {
	newLocalFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := mirrorDirectOrder(map[string]any{
		"id": float64(91), "account_email": "ma***@example.com", "plan": "plus",
		"status": "completed", "completed_at": now.Format(time.RFC3339),
	}, "zovo_webhook", now.Unix()); err != nil {
		t.Fatal(err)
	}
	var email string
	if err := db.DB.QueryRow("SELECT account_email FROM zovo_direct_order_mirror WHERE upstream_id=91").Scan(&email); err != nil {
		t.Fatal(err)
	}
	if email != "" {
		t.Fatal("masked email was treated as a customer identity")
	}
}
