package handler

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func TestEnrolmentDoesNotReplaceCardsBecauseOfLegacyCounters(t *testing.T) {
	newLocalFixture(t)
	settings := localSettings{MaxSuccessfulPaymentsPerCard: 3}
	for i := int64(1); i <= 20; i++ {
		settings.CardIDs = append(settings.CardIDs, i)
	}
	raw, _ := json.Marshal(settings)
	if err := db.SetSetting("local_cdk_settings", string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := enrollVerifiedCard(99); err == nil {
		t.Fatal("full active pool was overwritten")
	}
	for _, id := range []int64{1, 2} {
		for i := 0; i < 3; i++ {
			_, err := db.DB.Exec("INSERT INTO local_cdks(code_hash,prefix,plan,status,expires_at,created_at,card_id) VALUES(?, 'test','plus','consumed',0,0,?)", fmt.Sprintf("done-%d-%d", id, i), id)
			if err != nil {
				t.Fatal(err)
			}
		}
		_, err := db.DB.Exec("UPDATE local_card_cycles SET success_limit=3,success_count=3,cooldown_until=9999999999 WHERE card_id=?", id)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := enrollVerifiedCard(99); err == nil {
		t.Fatal("legacy capped counters unexpectedly caused a card replacement")
	}
	got := localCardIDs(readLocalSettings())
	if len(got) != 20 {
		t.Fatal("wrong pool size", len(got))
	}
	seen := map[int64]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if !seen[1] || !seen[2] || seen[99] {
		t.Fatal("legacy counters changed the active random pool", got)
	}
	var historical int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM local_cdks WHERE card_id=2 AND status='consumed'").Scan(&historical); err != nil || historical != 3 {
		t.Fatal("usage history lost", err)
	}
	if err := enrollVerifiedCard(1); err != nil {
		t.Fatal("enrolling an existing card was not idempotent", err)
	}
}
