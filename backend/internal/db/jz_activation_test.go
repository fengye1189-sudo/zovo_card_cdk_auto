package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func openManagedActivationTestDB(t *testing.T) {
	t.Helper()
	database, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "managed.db")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	previous := DB
	DB = database
	t.Cleanup(func() {
		_ = database.Close()
		DB = previous
	})
	if err := InitManagedActivation(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedActivationClaimsOnlyOnceWithoutStoringSecrets(t *testing.T) {
	openManagedActivationTestDB(t)
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	if err := BeginManagedActivation("code-hash", "attempt-hash", "plus", now); err != nil {
		t.Fatal(err)
	}
	if err := SaveManagedPreflight("code-hash", "attempt-hash", "preflight-hash", "session-hash", "member@example.com", now); err != nil {
		t.Fatal(err)
	}
	if err := ClaimManagedActivation("code-hash", "attempt-hash", "preflight-hash", "session-hash", now); err != nil {
		t.Fatal(err)
	}
	if err := ClaimManagedActivation("code-hash", "attempt-hash", "preflight-hash", "session-hash", now); !errors.Is(err, ErrManagedSubmitClaimed) {
		t.Fatalf("second claim error=%v", err)
	}
	row, err := GetManagedActivationByCodeHash("code-hash")
	if err != nil {
		t.Fatal(err)
	}
	if row.QueryExpiresAt != now.Add(7*24*time.Hour).Unix() || row.Status != "submitted" {
		t.Fatalf("row=%+v", row)
	}
	var schema string
	if err := DB.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='managed_activation_attempts'").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"session_json", "cdk_code", "plaintext_code"} {
		if strings.Contains(strings.ToLower(schema), strings.ToLower(forbidden)) {
			t.Fatalf("schema stores forbidden secret field %q: %s", forbidden, schema)
		}
	}
}

func TestManagedActivationAllowsRetryOnlyAfterConfirmedFailure(t *testing.T) {
	openManagedActivationTestDB(t)
	now := time.Now().UTC()
	if err := BeginManagedActivation("code-hash", "attempt-1", "plus", now); err != nil {
		t.Fatal(err)
	}
	if err := SaveManagedPreflight("code-hash", "attempt-1", "pf", "session", "", now); err != nil {
		t.Fatal(err)
	}
	if err := ClaimManagedActivation("code-hash", "attempt-1", "pf", "session", now); err != nil {
		t.Fatal(err)
	}
	if err := BeginManagedActivation("code-hash", "attempt-2", "plus", now); !errors.Is(err, ErrManagedAttemptActive) {
		t.Fatalf("active retry error=%v", err)
	}
	if err := RecordManagedActivationResult("code-hash", "TASK-1", "plus", "failed", "", "not completed", now); err != nil {
		t.Fatal(err)
	}
	if err := BeginManagedActivation("code-hash", "attempt-2", "plus", now.Add(time.Minute)); err != nil {
		t.Fatalf("confirmed-failure retry=%v", err)
	}
}
