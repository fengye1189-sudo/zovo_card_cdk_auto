package handler

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func TestPlusUsesRemainAvailableWithoutCooldown(t *testing.T) {
	f := newLocalFixture(t)
	now := time.Now().Unix()
	if _, err := ensureLocalCardCycle(123, "ordinary", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec("UPDATE local_card_cycles SET success_limit=3,success_count=3,cooldown_until=? WHERE card_id=123", now+86400); err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 2; id++ {
		if _, err := db.DB.Exec("INSERT INTO local_cdks(id,code_hash,prefix,plan,status,expires_at,created_at,card_id) VALUES(?,?,'plus','plus','reserved',0,0,123)", id, string(rune('a'+id))); err != nil {
			t.Fatal(err)
		}
		if err := recordAuthoritativeLocalStatus(id, "consumed", "done", now); err != nil {
			t.Fatal(err)
		}
	}
	var count, limit int
	var cooldown int64
	if err := db.DB.QueryRow("SELECT success_count,success_limit,cooldown_until FROM local_card_cycles WHERE card_id=123").Scan(&count, &limit, &cooldown); err != nil {
		t.Fatal(err)
	}
	if count != 5 || limit != localCardUnlimitedLimit || cooldown != 0 {
		t.Fatalf("card retained old cap: count=%d limit=%d cooldown=%d", count, limit, cooldown)
	}
	f.plan = "plus"
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/", nil)
	ids, err := localAvailableCards(c, cardplatform.NewFromSettings(), localSettings{CardIDs: []int64{123}, MinCardBalanceMinor: 100}, "plus")
	if err != nil || len(ids) != 1 || ids[0] != 123 {
		t.Fatal("eligible card was not reusable", ids, err)
	}
}

func TestGoDoesNotChangePlusSuccessHistory(t *testing.T) {
	newLocalFixture(t)
	now := time.Now().Unix()
	if _, err := ensureLocalCardCycle(123, "ordinary", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec("UPDATE local_card_cycles SET success_count=7 WHERE card_id=123"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec("INSERT INTO local_cdks(id,code_hash,prefix,plan,status,expires_at,created_at,card_id) VALUES(1,'go-random','go','go','reserved',0,0,123)"); err != nil {
		t.Fatal(err)
	}
	if err := recordAuthoritativeLocalStatus(1, "consumed", "done", now); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.DB.QueryRow("SELECT success_count FROM local_card_cycles WHERE card_id=123").Scan(&count); err != nil || count != 7 {
		t.Fatal("Go changed Plus history", count, err)
	}
}

func TestNoCooldownMigrationRunsIdempotently(t *testing.T) {
	newLocalFixture(t)
	now := time.Now().Unix()
	if _, err := db.DB.Exec("INSERT INTO local_card_cycles VALUES(123,'ordinary',3,3,?,?,?)", now, now+7*86400, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec("INSERT OR REPLACE INTO automation_card_lifecycle(card_id,phase,retire_state,retire_reason,created_at,updated_at) VALUES(123,'final','queued','final_two_successes',?,?)", now, now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := db.InitLocalCDK(); err != nil {
			t.Fatal(err)
		}
	}
	var limit int
	var cooldown int64
	var phase, state, reason string
	if err := db.DB.QueryRow("SELECT success_limit,cooldown_until FROM local_card_cycles WHERE card_id=123").Scan(&limit, &cooldown); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow("SELECT phase,retire_state,retire_reason FROM automation_card_lifecycle WHERE card_id=123").Scan(&phase, &state, &reason); err != nil {
		t.Fatal(err)
	}
	if limit != localCardUnlimitedLimit || cooldown != 0 || phase != "primary" || state != "active" || reason != "" {
		t.Fatal("no-cooldown migration failed", limit, cooldown, phase, state, reason)
	}
}

func TestExactDeclineStillRemovesCardFromRandomPool(t *testing.T) {
	newLocalFixture(t)
	now := time.Now().Unix()
	if _, err := ensureLocalCardCycle(123, "ordinary", now); err != nil {
		t.Fatal(err)
	}
	if err := recordAutomationDecline(1, 123, "declined", now); err != nil {
		t.Fatal(err)
	}
	ok, err := localCardHasCycleCapacity(123, "ordinary", now)
	if err != nil || ok {
		t.Fatal("declined card remained eligible", ok, err)
	}
}

func TestPro5xCompletionReleasesDedicatedCardWithoutUsageCap(t *testing.T) {
	newLocalFixture(t)
	now := time.Now().Unix()
	if _, err := db.DB.Exec("INSERT INTO local_cdks(id,code_hash,prefix,plan,status,expires_at,created_at,card_id) VALUES(1,'pro5x-history','PRO5X-ABCD','pro_5x','reserved',?,?,789)", now+86400, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec("INSERT INTO pro_dedicated_orders(local_id,card_id,money_id,state,created_at) VALUES(1,789,'pro5x-money','submitted',?)", now); err != nil {
		t.Fatal(err)
	}
	if ids, _, err := localPoolCards(localSettings{}, now); err != nil || len(ids) != 0 {
		t.Fatal("unfinished Pro 5X card entered pool", ids, err)
	}
	if err := recordAuthoritativeLocalStatus(1, "consumed", "done", now); err != nil {
		t.Fatal(err)
	}
	var state, kind string
	var limit int
	if err := db.DB.QueryRow("SELECT p.state,c.card_kind,c.success_limit FROM pro_dedicated_orders p JOIN local_card_cycles c ON c.card_id=p.card_id WHERE p.local_id=1").Scan(&state, &kind, &limit); err != nil || state != "completed" || kind != "ordinary" || limit != localCardUnlimitedLimit {
		t.Fatal("successful Pro 5X card did not enter unlimited pool", state, kind, limit, err)
	}
	if ids, kinds, err := localPoolCards(localSettings{}, now); err != nil || len(ids) != 1 || ids[0] != 789 || kinds[789] != "ordinary" {
		t.Fatal("successful Pro 5X card did not enter shared pool", ids, kinds, err)
	}
}

func TestMigratedProHistoryStillRequiresUpstreamEligibility(t *testing.T) {
	f := newLocalFixture(t)
	if _, err := db.DB.Exec("INSERT INTO local_cdks(id,code_hash,prefix,plan,status,expires_at,created_at,card_id) VALUES(1,'pro-ok','pro','pro_20x','consumed',0,0,789),(2,'pro-review','pro','pro_20x','review',0,0,790)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec("INSERT INTO pro_dedicated_orders(local_id,card_id,money_id,state,created_at) VALUES(1,789,'pro-ok-money','submitted',0),(2,790,'pro-review-money','submitted',0)"); err != nil {
		t.Fatal(err)
	}
	if err := db.InitLocalCDK(); err != nil {
		t.Fatal(err)
	}
	var reviewCycles int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM local_card_cycles WHERE card_id=790").Scan(&reviewCycles); err != nil || reviewCycles != 0 {
		t.Fatal("review Pro history was made reusable", reviewCycles, err)
	}
	f.plan = "plus"
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequestWithContext(context.Background(), "GET", "/", nil)
	ids, err := localAvailableCards(c, cardplatform.NewFromSettings(), localSettings{MinCardBalanceMinor: 100}, "plus")
	if err != nil || len(ids) != 0 {
		t.Fatal("migration bypassed upstream eligibility", ids, err)
	}
}
