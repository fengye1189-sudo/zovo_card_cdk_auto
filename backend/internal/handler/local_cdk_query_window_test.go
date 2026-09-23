package handler

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func TestSubmittedLocalPreviewAndResultRespectSevenDayWindow(t *testing.T) {
	f := newLocalFixture(t)
	code := f.code(t)
	const (
		fullEmail       = "private.customer@example.com"
		internalMessage = "INTERNAL card=424242 token=do-not-leak"
	)

	if _, err := db.DB.Exec(`
		UPDATE local_cdks
		SET status='consumed', email=?, message=?
		WHERE id=1
	`, fullEmail, internalMessage); err != nil {
		t.Fatal(err)
	}

	// Handler coverage uses a stable margin. Exact T-1 and T boundary semantics
	// are covered deterministically by TestLocalPublicQueryWindowLastSecondAndBoundary.
	submittedAt := time.Now().Unix() - int64(publicCDKQueryWindow/time.Second) + 30
	if _, err := db.DB.Exec(`
		INSERT INTO local_card_selections(local_id, card_id, selected_at)
		VALUES (1, 123, ?)
	`, submittedAt); err != nil {
		t.Fatal(err)
	}

	status, preview := f.call("/preview", gin.H{"code": code})
	if status != 200 {
		t.Fatalf("active-window preview status=%d response=%v", status, preview)
	}
	token, _ := preview["redemption_token"].(string)
	if token == "" {
		t.Fatalf("active-window preview did not issue a result token: %v", preview)
	}
	var tokenExpires int64
	if err := db.DB.QueryRow(`SELECT token_expires FROM local_cdks WHERE id=1`).Scan(&tokenExpires); err != nil {
		t.Fatal(err)
	}
	remaining := tokenExpires - time.Now().Unix()
	if remaining < 3599 || remaining > 3600 {
		t.Fatalf("result token lifetime=%ds, want one hour", remaining)
	}

	status, result := f.call("/result", gin.H{"redemption_token": token})
	if status != 200 || result["status"] != "completed" {
		t.Fatalf("active-window result status=%d response=%v", status, result)
	}
	encoded := strings.TrimSpace(toJSON(result))
	if strings.Contains(encoded, fullEmail) || strings.Contains(encoded, internalMessage) || strings.Contains(encoded, "do-not-leak") {
		t.Fatalf("local result leaked full identity or internal message: %s", encoded)
	}
	if result["email"] != maskEmail(fullEmail) || result["message"] != "兑换已完成" {
		t.Fatalf("local result was not safely normalized: %v", result)
	}

	var previousTokenHash string
	if err := db.DB.QueryRow(`SELECT token_hash FROM local_cdks WHERE id=1`).Scan(&previousTokenHash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`
		UPDATE local_card_selections
		SET selected_at=?
		WHERE local_id=1
	`, time.Now().Unix()-int64(publicCDKQueryWindow/time.Second)); err != nil {
		t.Fatal(err)
	}

	status, preview = f.call("/preview", gin.H{"code": code})
	if status != 410 || preview["status"] != "query_expired" {
		t.Fatalf("T boundary preview status=%d response=%v", status, preview)
	}
	if _, exists := preview["redemption_token"]; exists {
		t.Fatalf("expired preview returned a token: %v", preview)
	}
	var afterTokenHash string
	if err := db.DB.QueryRow(`SELECT token_hash FROM local_cdks WHERE id=1`).Scan(&afterTokenHash); err != nil {
		t.Fatal(err)
	}
	if afterTokenHash != previousTokenHash {
		t.Fatalf("expired preview changed token hash: before=%q after=%q", previousTokenHash, afterTokenHash)
	}

	status, result = f.call("/result", gin.H{"redemption_token": token})
	if status != 410 || result["status"] != "query_expired" {
		t.Fatalf("T boundary result status=%d response=%v", status, result)
	}
}

func TestUnusedLocalPreviewStillIssuesToken(t *testing.T) {
	f := newLocalFixture(t)
	code := f.code(t)

	status, preview := f.call("/preview", gin.H{"code": code})
	if status != 200 {
		t.Fatalf("unused preview status=%d response=%v", status, preview)
	}
	if token, _ := preview["redemption_token"].(string); token == "" {
		t.Fatalf("unused preview did not issue token: %v", preview)
	}
	if preview["status"] != "unused" {
		t.Fatalf("unused preview status was changed: %v", preview)
	}
}

func TestLocalQueryWindowUsesEarliestPositiveAnchorAcrossAllSources(t *testing.T) {
	f := newLocalFixture(t)
	code := f.code(t)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	selectionAt := now.Add(-2 * 24 * time.Hour).Unix()
	proCreatedAt := now.Add(-8 * 24 * time.Hour).Unix()
	watchFirstSeen := now.Add(-4 * 24 * time.Hour).Unix()
	if _, err := db.DB.Exec(`
		UPDATE local_cdks SET status='consumed' WHERE id=1;
		INSERT INTO local_card_selections(local_id, card_id, selected_at)
		VALUES (1, 123, ?);
		INSERT INTO pro_dedicated_orders(local_id, card_id, money_id, state, created_at)
		VALUES (1, 456, 'earliest-anchor-money', 'submitted', ?);
		INSERT INTO automation_watch(local_id, first_seen)
		VALUES (1, ?)
	`, selectionAt, proCreatedAt, watchFirstSeen); err != nil {
		t.Fatal(err)
	}

	submittedAt, expiresAt, err := localQueryWindowByID(1, now)
	if !errors.Is(err, errLocalCDKQueryExpired) {
		t.Fatalf("earliest source window error=%v, want errLocalCDKQueryExpired", err)
	}
	if submittedAt != proCreatedAt || expiresAt != proCreatedAt+int64(publicCDKQueryWindow/time.Second) {
		t.Fatalf("window used a later source: submitted=%d expires=%d want=%d/%d",
			submittedAt, expiresAt, proCreatedAt, proCreatedAt+int64(publicCDKQueryWindow/time.Second))
	}

	loaded, err := loadLocalPublicCDK(code, now)
	if !errors.Is(err, errLocalCDKQueryExpired) {
		t.Fatalf("public local lookup error=%v, want errLocalCDKQueryExpired", err)
	}
	if loaded == nil || loaded.SubmittedAt != proCreatedAt || loaded.QueryExpiresAt != expiresAt {
		t.Fatalf("public local lookup did not preserve earliest anchor: %+v", loaded)
	}
}

func toJSON(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
