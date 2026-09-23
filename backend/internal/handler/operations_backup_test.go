package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func newOperationsFixture(t *testing.T) *operationsManager {
	t.Helper()
	newLocalFixture(t)
	if err := initOperationsSchema(db.DB); err != nil {
		t.Fatal(err)
	}
	return &operationsManager{database: db.DB, folder: filepath.Join(t.TempDir(), "backups")}
}

func seedOperationsCode(t *testing.T, hash, status string, card int64) {
	t.Helper()
	_, err := db.DB.Exec("INSERT INTO local_cdks(code_hash,prefix,plan,status,expires_at,created_at,card_id) VALUES(?,?,'plus',?,?,?,?)", hash, hash, status, time.Now().Unix()+86400, time.Now().Unix(), card)
	if err != nil {
		t.Fatal(err)
	}
}

func TestOperationsSnapshotIncludesWALAndRestoresIndependently(t *testing.T) {
	m := newOperationsFixture(t)
	if err := db.SetSetting("restore_test_marker", "original-private-setting"); err != nil {
		t.Fatal(err)
	}
	seedOperationsCode(t, "complete-a", "consumed", 123)
	seedOperationsCode(t, "inflight-b", "review", 123)
	_, err := db.DB.Exec("INSERT INTO automation_money(id,action,amount_minor,reserved_minor,state,created_at) VALUES('locked','open',100,120,'unknown',?)", time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	backup, err := m.create(context.Background(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	if backup.Codes != 2 || backup.Completed != 1 || backup.Unresolved != 1 || backup.MoneyHolds != 1 {
		t.Fatalf("incomplete recovery counts: %+v", backup)
	}
	if err := db.SetSetting("restore_test_marker", "updated-live-setting"); err != nil {
		t.Fatal(err)
	}
	seedOperationsCode(t, "live-only", "consumed", 456)
	verified, err := m.verify(context.Background(), backup.Name)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Codes != 2 || verified.SHA256 != backup.SHA256 {
		t.Fatal("backup changed with live database")
	}
	file, err := m.file(backup.Name)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := sql.Open("sqlite3", sqliteFileURI(file, true))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var value string
	if err := restored.QueryRow("SELECT value FROM site_settings WHERE key='restore_test_marker'").Scan(&value); err != nil || value != "original-private-setting" {
		t.Fatal("WAL setting was not restored")
	}
	if value, err := db.GetSetting("restore_test_marker"); err != nil || value != "updated-live-setting" {
		t.Fatal("verification modified live settings")
	}
	raw, _ := json.Marshal(backup)
	if strings.Contains(string(raw), "private-setting") {
		t.Fatal("metadata leaked setting payload")
	}
	files, _ := os.ReadDir(m.folder)
	for _, file := range files {
		if strings.HasPrefix(file.Name(), ".restore-check-") || strings.HasSuffix(file.Name(), ".partial") {
			t.Fatal("temporary restore data retained")
		}
	}
}

func TestOperationsBackupRejectsCorruptionAndTraversal(t *testing.T) {
	m := newOperationsFixture(t)
	backup, err := m.create(context.Background(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../private.sqlite", backup.Name + ".json", "/etc/passwd", strings.ReplaceAll(backup.Name, "maple-", "../maple-")} {
		if _, err := m.file(name); err == nil {
			t.Fatalf("unsafe path accepted: %q", name)
		}
	}
	file, _ := m.file(backup.Name)
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("tampering")
	_ = f.Close()
	if _, err := m.verify(context.Background(), backup.Name); err == nil {
		t.Fatal("modified snapshot passed manifest checksum")
	}
}

func TestOperationsBackupRejectsSymlink(t *testing.T) {
	m := newOperationsFixture(t)
	if err := m.prepareFolder(); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "not-a-backup")
	if err := os.WriteFile(target, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	name := "maple-20260910T010203Z-123456abcdef.sqlite"
	if err := os.Symlink(target, filepath.Join(m.folder, name)); err != nil {
		t.Skip("symlink permission not available")
	}
	if _, err := m.file(name); err == nil {
		t.Fatal("symlink exposed as backup")
	}
}

func TestOperationsBackupRetentionAndCancellation(t *testing.T) {
	m := newOperationsFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.create(ctx, "manual"); err == nil {
		t.Fatal("cancelled snapshot succeeded")
	}
	for i := 0; i < operationsManualRetention+1; i++ {
		if _, err := m.create(context.Background(), "manual"); err != nil {
			t.Fatal(err)
		}
	}
	list, err := m.list()
	if err != nil || len(list) != operationsManualRetention {
		t.Fatal("retention limit not enforced", len(list), err)
	}
	for _, item := range list {
		if _, err := m.verify(context.Background(), item.Name); err != nil {
			t.Fatal("retained backup invalid", err)
		}
	}
}

func TestOperationsDailyBackupIsNotDuplicated(t *testing.T) {
	m := newOperationsFixture(t)
	operationsDailyBackup(context.Background(), m)
	_, _ = db.DB.Exec("UPDATE operations_runtime SET last_backup_attempt=0 WHERE id=1")
	operationsDailyBackup(context.Background(), m)
	list, err := m.list()
	if err != nil || len(list) != 1 || list[0].Kind != "daily" {
		t.Fatal("daily backup duplicated", len(list), err)
	}
}

func TestOperationsBackupVerifiesRequiredSettings(t *testing.T) {
	m := newOperationsFixture(t)
	_, _ = db.DB.Exec("UPDATE automation_policy SET value='not-json' WHERE id=1")
	if _, err := m.create(context.Background(), "manual"); err == nil {
		t.Fatal("invalid configuration marked restorable")
	}
	list, err := m.list()
	if err != nil || len(list) != 0 {
		t.Fatal("failed backup published")
	}
}

func TestOperationsHealthCountsCapAndPreservesAlertDedupe(t *testing.T) {
	m := newOperationsFixture(t)
	settings := localSettings{Enabled: true, CardIDs: []int64{123, 456, 789}, MaxSuccessfulPaymentsPerCard: 3}
	raw, _ := json.Marshal(settings)
	if err := db.SetSetting("local_cdk_settings", string(raw)); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{"a", "b", "c"} {
		seedOperationsCode(t, hash, "consumed", 123)
	}
	seedOperationsCode(t, "review", "review", 456)
	seedOperationsCode(t, "failed", "failed", 789)
	now := time.Now().Unix()
	if _, err := db.DB.Exec("INSERT INTO local_card_cycles(card_id,card_kind,success_limit,success_count,cycle_started_at,cooldown_until,updated_at) VALUES(123,'ordinary',3,3,?,?,?),(456,'ordinary',4,0,?,0,?),(789,'ordinary',3,0,?,0,?)", now, now+86400, now, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	report, err := collectOperationsHealth(context.Background(), m.database)
	if err != nil {
		t.Fatal(err)
	}
	if report.CappedCards != 0 || report.CandidateCards != 2 || report.Outstanding != 1 || report.Completed != 3 {
		t.Fatalf("incorrect health counts: %+v", report)
	}
	if report.Cards[0].Remaining != -1 || report.Cards[2].Remaining != -1 {
		t.Fatal("legacy payment counters still limit card selection")
	}
	operationsSetAlert("test", true, "unchanged condition")
	_, _ = db.DB.Exec("UPDATE automation_alerts SET updated_at=123 WHERE alert_key='operations:test'")
	operationsSetAlert("test", true, "unchanged condition")
	var stamp int64
	if err := db.DB.QueryRow("SELECT updated_at FROM automation_alerts WHERE alert_key='operations:test'").Scan(&stamp); err != nil || stamp != 123 {
		t.Fatal("unchanged alert refreshed instead of deduping")
	}
	operationsSetAlert("test", false, "")
	operationsSetAlert("test", true, "unchanged condition")
	var resolved int
	_ = db.DB.QueryRow("SELECT resolved FROM automation_alerts WHERE alert_key='operations:test'").Scan(&resolved)
	if resolved != 0 {
		t.Fatal("recurring condition was not reactivated")
	}
}
