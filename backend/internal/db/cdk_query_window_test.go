package db

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func openCDKQueryTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "query-window.db")+"?_busy_timeout=10000")
	if err != nil {
		t.Fatal(err)
	}
	previous := DB
	DB = database
	t.Cleanup(func() {
		_ = database.Close()
		DB = previous
	})
	return database
}

func createLegacyCDKQueryTables(t *testing.T, database *sql.DB) {
	t.Helper()
	statements := []string{
		`CREATE TABLE cdk_session_bindings (
			cdk_code TEXT PRIMARY KEY,
			session_payload TEXT NOT NULL,
			redemption_token TEXT,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE recharge_tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id TEXT UNIQUE NOT NULL,
			cdk_code TEXT NOT NULL,
			session_json TEXT,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE cdk_preflight_grants (
			attempt_nonce TEXT NOT NULL,
			token_hash TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY(attempt_nonce, token_hash)
		)`,
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrateCDKBindingQueryWindowBackfillsOnlyLegacyRowsOnce(t *testing.T) {
	database := openCDKQueryTestDB(t)
	createLegacyCDKQueryTables(t, database)

	if _, err := database.Exec(`
		INSERT INTO cdk_session_bindings
			(cdk_code, session_payload, redemption_token, updated_at)
		VALUES ('PULS-LEGACY', 'session-secret', 'legacy-token', '2026-09-01 12:30:00')
	`); err != nil {
		t.Fatal(err)
	}
	if err := migrateCDKBindingQueryWindow(); err != nil {
		t.Fatal(err)
	}

	legacy, err := GetBindingByCDK("puls-legacy")
	if err != nil {
		t.Fatal(err)
	}
	wantStarted := time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC).Unix()
	if legacy.QueryStartedAt != wantStarted {
		t.Fatalf("started_at=%d, want %d", legacy.QueryStartedAt, wantStarted)
	}
	if legacy.QueryExpiresAt != wantStarted+int64(cdkPublicQueryWindow/time.Second) {
		t.Fatalf("expires_at=%d", legacy.QueryExpiresAt)
	}

	if err := BindCDKRedemptionToken("PULS-PREVIEW", "preview-token"); err != nil {
		t.Fatal(err)
	}
	if err := migrateCDKBindingQueryWindow(); err != nil {
		t.Fatal(err)
	}
	preview, err := GetBindingByCDK("PULS-PREVIEW")
	if err != nil {
		t.Fatal(err)
	}
	if preview.QueryStartedAt != 0 || preview.QueryExpiresAt != 0 {
		t.Fatalf("preview-only row was incorrectly activated: %+v", preview)
	}
	if _, err := GetPublicCDKBindingByCode("PULS-PREVIEW", time.Now()); !errors.Is(err, ErrCDKQueryNotStarted) {
		t.Fatalf("public preview error=%v, want ErrCDKQueryNotStarted", err)
	}
}

func TestMigrateCDKBindingQueryWindowRejectsDuplicateRedemptionTokens(t *testing.T) {
	database := openCDKQueryTestDB(t)
	createLegacyCDKQueryTables(t, database)
	if _, err := database.Exec(`
		INSERT INTO cdk_session_bindings(cdk_code, session_payload, redemption_token)
		VALUES
			('PULS-DUPLICATE-TOKEN-A', '', 'provider-duplicate-token'),
			('PULS-DUPLICATE-TOKEN-B', '', 'provider-duplicate-token')
	`); err != nil {
		t.Fatal(err)
	}
	if err := migrateCDKBindingQueryWindow(); err == nil {
		t.Fatal("strict binding migration accepted duplicate non-empty redemption tokens")
	}
}

func TestBindCDKRedemptionTokenCASRejectsConcurrentChanges(t *testing.T) {
	database := openCDKQueryTestDB(t)
	createLegacyCDKQueryTables(t, database)
	if err := migrateCDKBindingQueryWindow(); err != nil {
		t.Fatal(err)
	}

	t.Run("preview cannot overwrite concurrent redeem", func(t *testing.T) {
		const (
			code      = "PULS-CAS-REDEEM"
			oldToken  = "cas-redeem-original-token"
			newToken  = "cas-redeem-stale-preview-token"
			preflight = "cas-redeem-preflight-token"
		)
		if err := BindCDKRedemptionToken(code, oldToken); err != nil {
			t.Fatal(err)
		}
		snapshot, err := GetBindingByCDK(code)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`
			INSERT INTO recharge_tasks(task_id, cdk_code, session_json)
			VALUES ('cas-before-redeem', ?, 'current-session')
		`, code); err != nil {
			t.Fatal(err)
		}
		startedAt := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
		if err := RegisterCDKPreflightGrant(code, oldToken, snapshot.AttemptNonce, preflight, startedAt); err != nil {
			t.Fatal(err)
		}
		if err := ClaimCDKRedemptionAttempt(oldToken, snapshot.AttemptNonce, preflight, startedAt); err != nil {
			t.Fatal(err)
		}

		if err := BindCDKRedemptionTokenCAS(code, newToken, snapshot); !errors.Is(err, ErrCDKBindingChanged) {
			t.Fatalf("preview-vs-redeem CAS error=%v, want ErrCDKBindingChanged", err)
		}
		got, err := GetBindingByCDK(code)
		if err != nil {
			t.Fatal(err)
		}
		if got.RedemptionToken != oldToken || got.SubmitClaimedAt != startedAt.Unix() ||
			got.QueryStartedAt != startedAt.Unix() ||
			got.QueryExpiresAt != startedAt.Add(cdkPublicQueryWindow).Unix() {
			t.Fatalf("stale preview overwrote concurrent redeem: %+v", got)
		}
		var session string
		if err := database.QueryRow(`
			SELECT session_json FROM recharge_tasks WHERE task_id='cas-before-redeem'
		`).Scan(&session); err != nil {
			t.Fatal(err)
		}
		if session != "current-session" {
			t.Fatalf("stale preview scrubbed current task session: %q", session)
		}
	})

	t.Run("older preview cannot overwrite newer preview", func(t *testing.T) {
		const code = "PULS-CAS-PREVIEW"
		missingSnapshot, err := GetBindingByCDK(code)
		if err != nil || missingSnapshot != nil {
			t.Fatalf("initial snapshot=%+v err=%v", missingSnapshot, err)
		}
		if err := BindCDKRedemptionTokenCAS(code, "newer-preview-token", missingSnapshot); err != nil {
			t.Fatal(err)
		}
		if err := BindCDKRedemptionTokenCAS(code, "older-preview-token", missingSnapshot); !errors.Is(err, ErrCDKBindingChanged) {
			t.Fatalf("preview-vs-preview CAS error=%v, want ErrCDKBindingChanged", err)
		}
		got, err := GetBindingByCDK(code)
		if err != nil {
			t.Fatal(err)
		}
		if got.RedemptionToken != "newer-preview-token" || got.QueryStartedAt != 0 ||
			got.QueryExpiresAt != 0 {
			t.Fatalf("older preview overwrote newer preview: %+v", got)
		}
	})

	t.Run("preview cannot overwrite concurrent identity enrichment", func(t *testing.T) {
		const (
			code  = "PULS-CAS-EMAIL"
			token = "cas-email-original-token"
		)
		if err := BindCDKRedemptionToken(code, token); err != nil {
			t.Fatal(err)
		}
		snapshot, err := GetBindingByCDK(code)
		if err != nil {
			t.Fatal(err)
		}
		if err := BindCDKAccountEmail(code, token, "current.customer@example.com"); err != nil {
			t.Fatal(err)
		}
		afterEmail, err := GetBindingByCDK(code)
		if err != nil {
			t.Fatal(err)
		}
		if afterEmail.UpdatedAt != snapshot.UpdatedAt {
			t.Fatalf("identity enrichment refreshed preview TTL: before=%q after=%q", snapshot.UpdatedAt, afterEmail.UpdatedAt)
		}
		if err := BindCDKRedemptionTokenCAS(code, "cas-email-stale-preview-token", snapshot); !errors.Is(err, ErrCDKBindingChanged) {
			t.Fatalf("email-change CAS error=%v, want ErrCDKBindingChanged", err)
		}
		got, err := GetBindingByCDK(code)
		if err != nil {
			t.Fatal(err)
		}
		if got.RedemptionToken != token || got.AccountEmail != "current.customer@example.com" {
			t.Fatalf("stale preview overwrote enriched identity: %+v", got)
		}
	})

	t.Run("preview cannot replace an already active attempt", func(t *testing.T) {
		const (
			code  = "PULS-CAS-ACTIVE"
			token = "cas-active-original-token"
		)
		if err := BindCDKRedemptionToken(code, token); err != nil {
			t.Fatal(err)
		}
		startedAt := time.Date(2026, 9, 19, 13, 0, 0, 0, time.UTC)
		if err := ActivateCDKQueryWindowByToken(token, startedAt); err != nil {
			t.Fatal(err)
		}
		activeSnapshot, err := GetBindingByCDK(code)
		if err != nil {
			t.Fatal(err)
		}
		if err := BindCDKRedemptionTokenCAS(code, "cas-active-new-token", activeSnapshot); !errors.Is(err, ErrCDKQueryAlreadyStarted) {
			t.Fatalf("active preview CAS error=%v, want ErrCDKQueryAlreadyStarted", err)
		}
		got, err := GetBindingByCDK(code)
		if err != nil {
			t.Fatal(err)
		}
		if got.RedemptionToken != token || got.QueryStartedAt != startedAt.Unix() ||
			got.QueryExpiresAt != startedAt.Add(cdkPublicQueryWindow).Unix() {
			t.Fatalf("active preview changed immutable window: %+v", got)
		}
	})
}

func TestAttemptNoncePreventsSameTokenABAEmailWrite(t *testing.T) {
	database := openCDKQueryTestDB(t)
	createLegacyCDKQueryTables(t, database)
	if err := migrateCDKBindingQueryWindow(); err != nil {
		t.Fatal(err)
	}
	const (
		code  = "PULS-ATTEMPT-NONCE-ABA"
		token = "provider-reused-opaque-token"
	)
	if err := BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	first, err := GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if first.AttemptNonce == "" {
		t.Fatal("first preview did not create an attempt nonce")
	}
	if err := BindCDKRedemptionTokenCAS(code, token, first); err != nil {
		t.Fatal(err)
	}
	second, err := GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if second.AttemptNonce == "" || second.AttemptNonce == first.AttemptNonce {
		t.Fatalf("same-token preview did not rotate attempt nonce: first=%q second=%q",
			first.AttemptNonce, second.AttemptNonce)
	}
	if second.QueryStartedAt != 0 || second.QueryExpiresAt != 0 {
		t.Fatalf("same-token preview started a query window: %+v", second)
	}
	if err := BindCDKAccountEmailForAttempt(code, token, first.AttemptNonce, "old.attempt@example.com"); !errors.Is(err, ErrCDKBindingChanged) {
		t.Fatalf("stale attempt email error=%v, want ErrCDKBindingChanged", err)
	}
	afterStale, err := GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if afterStale.AccountEmail != "" || afterStale.AttemptNonce != second.AttemptNonce {
		t.Fatalf("stale attempt wrote identity into new attempt: %+v", afterStale)
	}
	if err := BindCDKAccountEmailForAttempt(code, token, second.AttemptNonce, "current.attempt@example.com"); err != nil {
		t.Fatal(err)
	}
	current, err := GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if current.AccountEmail != "current.attempt@example.com" || current.AttemptNonce != second.AttemptNonce {
		t.Fatalf("current attempt email was not saved: %+v", current)
	}
	if current.UpdatedAt != second.UpdatedAt {
		t.Fatalf("email write refreshed attempt TTL: before=%q after=%q", second.UpdatedAt, current.UpdatedAt)
	}
}

func TestPreflightGrantAndSubmitClaimAreBoundToExactAttempt(t *testing.T) {
	database := openCDKQueryTestDB(t)
	createLegacyCDKQueryTables(t, database)
	if err := migrateCDKBindingQueryWindow(); err != nil {
		t.Fatal(err)
	}
	const (
		code           = "PULS-CLAIM-BOUND-ATTEMPT"
		redemption     = "claim-bound-redemption-token"
		preflightToken = "claim-bound-preflight-token"
		secondGrant    = "claim-bound-second-preflight-token"
	)
	if err := BindCDKRedemptionToken(code, redemption); err != nil {
		t.Fatal(err)
	}
	pending, err := GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	if err := RegisterCDKPreflightGrant(code, redemption, pending.AttemptNonce, preflightToken, now); err != nil {
		t.Fatal(err)
	}
	if err := RegisterCDKPreflightGrant(code, redemption, pending.AttemptNonce, secondGrant, now); err != nil {
		t.Fatal(err)
	}
	var grantCount int
	var hashes string
	if err := database.QueryRow(`
		SELECT COUNT(*), COALESCE(GROUP_CONCAT(token_hash, ','), '')
		FROM cdk_preflight_grants WHERE attempt_nonce=?
	`, pending.AttemptNonce).Scan(&grantCount, &hashes); err != nil {
		t.Fatal(err)
	}
	if grantCount != 2 || strings.Contains(hashes, preflightToken) || strings.Contains(hashes, secondGrant) {
		t.Fatalf("preflight grants were not stored as two hashes: count=%d hashes=%q", grantCount, hashes)
	}
	if err := ClaimCDKRedemptionAttempt(redemption, pending.AttemptNonce, "wrong-preflight-token", now); !errors.Is(err, ErrCDKBindingMismatch) {
		t.Fatalf("wrong preflight grant error=%v, want ErrCDKBindingMismatch", err)
	}
	if err := ClaimCDKRedemptionAttempt(redemption, "wrong-attempt-token", preflightToken, now); !errors.Is(err, ErrCDKBindingMismatch) {
		t.Fatalf("wrong attempt handle error=%v, want ErrCDKBindingMismatch", err)
	}
	unclaimed, err := GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if unclaimed.SubmitClaimedAt != 0 || unclaimed.QueryStartedAt != 0 || unclaimed.QueryExpiresAt != 0 {
		t.Fatalf("invalid grant or attempt mutated pending binding: %+v", unclaimed)
	}
	if err := ClaimCDKRedemptionAttempt(redemption, pending.AttemptNonce, preflightToken, now); err != nil {
		t.Fatal(err)
	}
	claimed, err := GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.SubmitClaimedAt != now.Unix() || claimed.QueryStartedAt != now.Unix() ||
		claimed.QueryExpiresAt != now.Add(cdkPublicQueryWindow).Unix() {
		t.Fatalf("claim did not start exactly one immutable window: %+v", claimed)
	}
	if err := ClaimCDKRedemptionAttempt(redemption, pending.AttemptNonce, secondGrant, now.Add(time.Second)); !errors.Is(err, ErrCDKSubmitAlreadyClaimed) {
		t.Fatalf("duplicate claim error=%v, want ErrCDKSubmitAlreadyClaimed", err)
	}
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM cdk_preflight_grants WHERE attempt_nonce=?
	`, pending.AttemptNonce).Scan(&grantCount); err != nil {
		t.Fatal(err)
	}
	if grantCount != 0 {
		t.Fatalf("successful claim retained %d preflight grants", grantCount)
	}
	if _, err := GetPublicCDKBindingByCode(code, now.Add(cdkPublicQueryWindow)); !errors.Is(err, ErrCDKQueryExpired) {
		t.Fatalf("exact claim expiry error=%v, want ErrCDKQueryExpired", err)
	}
	expired, err := GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if expired.RedemptionToken != "" || expired.AttemptNonce != "" || expired.AccountEmail != "" ||
		expired.SubmitClaimedAt != now.Unix() || expired.QueryStartedAt != now.Unix() ||
		expired.QueryExpiresAt != now.Add(cdkPublicQueryWindow).Unix() {
		t.Fatalf("expired claim did not scrub credentials while retaining its tombstone: %+v", expired)
	}
}

func TestPreflightGrantRetentionCapAndIndependentClaimCandidates(t *testing.T) {
	database := openCDKQueryTestDB(t)
	createLegacyCDKQueryTables(t, database)
	if err := migrateCDKBindingQueryWindow(); err != nil {
		t.Fatal(err)
	}

	t.Run("more than sixteen grants remain bounded and newest is claimable", func(t *testing.T) {
		const (
			code  = "PULS-GRANT-CAP"
			token = "grant-cap-redemption-token"
		)
		if err := BindCDKRedemptionToken(code, token); err != nil {
			t.Fatal(err)
		}
		pending, err := GetBindingByCDK(code)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		var newest string
		for i := 0; i < cdkPreflightGrantLimit+5; i++ {
			newest = fmt.Sprintf("preflight-grant-%02d", i)
			if err := RegisterCDKPreflightGrant(code, token, pending.AttemptNonce, newest, now.Add(time.Duration(i)*time.Second)); err != nil {
				t.Fatalf("register grant %d: %v", i, err)
			}
		}
		var count int
		if err := database.QueryRow(`
			SELECT COUNT(*) FROM cdk_preflight_grants WHERE attempt_nonce=?
		`, pending.AttemptNonce).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != cdkPreflightGrantLimit {
			t.Fatalf("retained grants=%d, want exactly %d", count, cdkPreflightGrantLimit)
		}
		if err := ClaimCDKRedemptionAttempt(token, pending.AttemptNonce, newest, now.Add(time.Minute)); err != nil {
			t.Fatalf("newest retained grant was not claimable: %v", err)
		}
	})

	t.Run("either of two grants may win but only one claim succeeds", func(t *testing.T) {
		const (
			code   = "PULS-TWO-GRANT-CLAIM"
			token  = "two-grant-redemption-token"
			grantA = "two-grant-candidate-a"
			grantB = "two-grant-candidate-b"
		)
		if err := BindCDKRedemptionToken(code, token); err != nil {
			t.Fatal(err)
		}
		pending, err := GetBindingByCDK(code)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		for _, grant := range []string{grantA, grantB} {
			if err := RegisterCDKPreflightGrant(code, token, pending.AttemptNonce, grant, now); err != nil {
				t.Fatal(err)
			}
		}

		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for _, grant := range []string{grantA, grantB} {
			grant := grant
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results <- ClaimCDKRedemptionAttempt(token, pending.AttemptNonce, grant, now)
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		successes, duplicates := 0, 0
		for err := range results {
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrCDKSubmitAlreadyClaimed):
				duplicates++
			default:
				t.Fatalf("two-grant concurrent claim returned %v", err)
			}
		}
		if successes != 1 || duplicates != 1 {
			t.Fatalf("two-grant claim results: successes=%d duplicates=%d", successes, duplicates)
		}
	})
}

func TestCDKQueryWindowDoesNotRenewAndExpiresAtBoundary(t *testing.T) {
	database := openCDKQueryTestDB(t)
	createLegacyCDKQueryTables(t, database)
	if err := migrateCDKBindingQueryWindow(); err != nil {
		t.Fatal(err)
	}

	const (
		code    = "PULS-BOUNDARY"
		token   = "redemption-secret"
		session = "session-secret"
		email   = "customer@example.com"
	)
	if err := BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	if err := BindCDKSession(code, token, session); err != nil {
		t.Fatal(err)
	}
	if err := BindCDKAccountEmail(code, token, " Customer@Example.COM "); err != nil {
		t.Fatal(err)
	}
	if _, err := GetPublicCDKBindingByCode(code, time.Now()); !errors.Is(err, ErrCDKQueryNotStarted) {
		t.Fatalf("preflight data activated query window: %v", err)
	}

	startedAt := time.Now().UTC().Add(-8 * 24 * time.Hour).Truncate(time.Second)
	if err := ActivateCDKQueryWindowByToken(token, startedAt); err != nil {
		t.Fatal(err)
	}
	if err := ActivateCDKQueryWindowByToken(token, startedAt.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	binding, err := GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	wantExpiry := startedAt.Add(cdkPublicQueryWindow).Unix()
	if binding.QueryStartedAt != startedAt.Unix() || binding.QueryExpiresAt != wantExpiry {
		t.Fatalf("window changed after second activation: %+v", binding)
	}
	if binding.AccountEmail != email {
		t.Fatalf("email=%q, want %q", binding.AccountEmail, email)
	}

	if _, err := database.Exec(`
		INSERT INTO recharge_tasks (task_id, cdk_code, session_json)
		VALUES ('task-1', ?, 'task-session-secret')
	`, code); err != nil {
		t.Fatal(err)
	}
	active, err := GetPublicCDKBindingByToken(token, time.Unix(wantExpiry-1, 0))
	if err != nil {
		t.Fatalf("last active second: %v", err)
	}
	if active.SessionPayload != session || active.AccountEmail != email {
		t.Fatalf("active binding missing expected data: %+v", active)
	}

	if _, err := GetPublicCDKBindingByCode(code, time.Unix(wantExpiry, 0)); !errors.Is(err, ErrCDKQueryExpired) {
		t.Fatalf("boundary error=%v, want ErrCDKQueryExpired", err)
	}
	after, err := GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if after.SessionPayload != "" || after.RedemptionToken != "" ||
		after.AccountEmail != "" || after.AttemptNonce != "" {
		t.Fatalf("expired secrets were retained: %+v", after)
	}
	var taskSession sql.NullString
	if err := database.QueryRow(`SELECT session_json FROM recharge_tasks WHERE task_id='task-1'`).Scan(&taskSession); err != nil {
		t.Fatal(err)
	}
	if taskSession.Valid {
		t.Fatalf("recharge task session was retained: %q", taskSession.String)
	}

	// An expired attempt cannot be reactivated with its old token. A later
	// successful upstream preview may, however, create a distinct pending
	// attempt with a new token and a fresh, not-yet-started query window.
	if err := BindCDKRedemptionToken(code, "new-token"); err != nil {
		t.Fatal(err)
	}
	if err := BindCDKSession(code, "new-token", "new-session"); err != nil {
		t.Fatal(err)
	}
	if err := BindCDKAccountEmail(code, "new-token", "new@example.com"); err != nil {
		t.Fatal(err)
	}
	after, err = GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if after.SessionPayload != "new-session" || after.RedemptionToken != "new-token" ||
		after.AccountEmail != "new@example.com" || after.QueryStartedAt != 0 ||
		after.QueryExpiresAt != 0 {
		t.Fatalf("successful preview did not create a clean pending attempt: %+v", after)
	}
	if err := ActivateCDKQueryWindowByToken(token, time.Now()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired token reactivation error=%v, want sql.ErrNoRows", err)
	}
	if err := BindCDKSession("", "unknown-old-token", "new-session"); err != nil {
		t.Fatal(err)
	}
	if err := BindCDKAccountEmail("", "unknown-old-token", "new@example.com"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown email binding error=%v, want sql.ErrNoRows", err)
	}
	if err := ActivateCDKQueryWindowByToken("unknown-old-token", time.Now()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("token-only binding created a renewable row: %v", err)
	}
	if _, err := GetPublicCDKBindingByCode(code, time.Now()); !errors.Is(err, ErrCDKQueryNotStarted) {
		t.Fatalf("fresh preview state error=%v, want ErrCDKQueryNotStarted", err)
	}
}

func TestActiveCDKBindingUpdatesDoNotExtendWindow(t *testing.T) {
	database := openCDKQueryTestDB(t)
	createLegacyCDKQueryTables(t, database)
	if err := migrateCDKBindingQueryWindow(); err != nil {
		t.Fatal(err)
	}

	const code = "PULS-NONRENEW"
	const token = "stable-token"
	if err := BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	if err := BindCDKSession(code, token, "session-before-submit"); err != nil {
		t.Fatal(err)
	}
	if err := BindCDKAccountEmail(code, token, "before@example.com"); err != nil {
		t.Fatal(err)
	}
	before, err := GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if before.QueryStartedAt != 0 || before.QueryExpiresAt != 0 {
		t.Fatalf("binding data started a window: %+v", before)
	}

	startedAt := time.Now().UTC().Truncate(time.Second)
	if err := ActivateCDKQueryWindowByToken(token, startedAt); err != nil {
		t.Fatal(err)
	}
	if err := BindCDKRedemptionToken(code, token); !errors.Is(err, ErrCDKQueryAlreadyStarted) {
		t.Fatalf("active repeated preview error=%v, want ErrCDKQueryAlreadyStarted", err)
	}
	if err := BindCDKSession(code, token, "session-after-submit"); err != nil {
		t.Fatal(err)
	}
	if err := BindCDKAccountEmail(code, token, "before@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := BindCDKAccountEmail(code, token, "different@example.com"); !errors.Is(err, ErrCDKBindingChanged) {
		t.Fatalf("conflicting active identity error=%v, want ErrCDKBindingChanged", err)
	}
	after, err := GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if after.QueryStartedAt != startedAt.Unix() || after.QueryExpiresAt != startedAt.Add(cdkPublicQueryWindow).Unix() {
		t.Fatalf("binding update extended the window: %+v", after)
	}
	if after.AccountEmail != "before@example.com" {
		t.Fatalf("active identity was overwritten: %+v", after)
	}
}

func TestGetSessionByCDKRequiresActiveWindowAndPurgesAtBoundary(t *testing.T) {
	database := openCDKQueryTestDB(t)
	createLegacyCDKQueryTables(t, database)
	if err := migrateCDKBindingQueryWindow(); err != nil {
		t.Fatal(err)
	}

	const (
		activeCode    = "PULS-SESSION-ACTIVE"
		activeToken   = "active-session-token"
		activeSession = "active-session-secret"
	)
	if err := BindCDKRedemptionToken(activeCode, activeToken); err != nil {
		t.Fatal(err)
	}
	if err := BindCDKSession(activeCode, activeToken, activeSession); err != nil {
		t.Fatal(err)
	}
	if err := ActivateCDKQueryWindowByToken(activeToken, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := GetSessionByCDK(activeCode)
	if err != nil || got != activeSession {
		t.Fatalf("active GetSessionByCDK=(%q,%v), want active session", got, err)
	}

	const (
		expiredCode    = "PULS-SESSION-BOUNDARY"
		expiredToken   = "expired-session-token"
		expiredSession = "expired-session-secret"
		expiredEmail   = "expired.session@example.com"
	)
	if err := BindCDKRedemptionToken(expiredCode, expiredToken); err != nil {
		t.Fatal(err)
	}
	if err := BindCDKSession(expiredCode, expiredToken, expiredSession); err != nil {
		t.Fatal(err)
	}
	if err := BindCDKAccountEmail(expiredCode, expiredToken, expiredEmail); err != nil {
		t.Fatal(err)
	}
	exactBoundaryStart := time.Unix(
		time.Now().Unix()-int64(cdkPublicQueryWindow/time.Second), 0,
	)
	if err := ActivateCDKQueryWindowByToken(expiredToken, exactBoundaryStart); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
		INSERT INTO recharge_tasks(task_id, cdk_code, session_json)
		VALUES ('direct-session-boundary', ?, 'task-session-secret')
	`, expiredCode); err != nil {
		t.Fatal(err)
	}

	got, err = GetSessionByCDK(expiredCode)
	if err != nil || got != "" {
		t.Fatalf("boundary GetSessionByCDK=(%q,%v), want empty without error", got, err)
	}
	binding, err := GetBindingByCDK(expiredCode)
	if err != nil {
		t.Fatal(err)
	}
	if binding == nil || binding.SessionPayload != "" || binding.RedemptionToken != "" ||
		binding.AccountEmail != "" || binding.AttemptNonce != "" {
		t.Fatalf("boundary GetSessionByCDK retained secrets: %+v", binding)
	}
	var taskSession sql.NullString
	if err := database.QueryRow(`
		SELECT session_json FROM recharge_tasks WHERE task_id='direct-session-boundary'
	`).Scan(&taskSession); err != nil {
		t.Fatal(err)
	}
	if taskSession.Valid {
		t.Fatalf("boundary GetSessionByCDK retained task session: %q", taskSession.String)
	}
}

func TestPurgeAllExpiredCDKBindingsKeepsActiveAndBusinessRows(t *testing.T) {
	database := openCDKQueryTestDB(t)
	createLegacyCDKQueryTables(t, database)
	if err := migrateCDKBindingQueryWindow(); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	rows := []struct {
		code, token, email, session, nonce, updated string
		started, expires                            int64
	}{
		{"PULS-EXPIRED", "expired-token", "old@example.com", "expired-session", "expired-nonce", "2026-09-19 11:00:00", now.Add(-8 * 24 * time.Hour).Unix(), now.Unix()},
		{"PULS-ACTIVE", "active-token", "active@example.com", "active-session", "active-nonce", "2026-09-18 11:00:00", now.Add(-24 * time.Hour).Unix(), now.Add(6 * 24 * time.Hour).Unix()},
		// Exact 24-hour boundary is stale; one second newer is retained.
		{"PULS-PREFLIGHT-STALE", "stale-token", "stale@example.com", "stale-session", "stale-nonce", "2026-09-18 12:00:00", 0, 0},
		{"PULS-PREFLIGHT-RECENT", "recent-token", "recent@example.com", "recent-session", "recent-nonce", "2026-09-18 12:00:01", 0, 0},
	}
	for _, row := range rows {
		if _, err := database.Exec(`
			INSERT INTO cdk_session_bindings
				(cdk_code, session_payload, redemption_token, account_email, attempt_nonce,
				 query_started_at, query_expires_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, row.code, row.session, row.token, row.email, row.nonce, row.started, row.expires, row.updated); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`
			INSERT INTO recharge_tasks (task_id, cdk_code, session_json)
			VALUES (?, ?, ?)
		`, "task-"+row.code, row.code, row.session); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Exec(`
		INSERT INTO cdk_session_bindings
			(cdk_code, session_payload, redemption_token, account_email, attempt_nonce,
			 submit_claimed_at, query_started_at, query_expires_at, updated_at)
		VALUES ('PULS-CLAIMED-NO-WINDOW', '', 'claimed-token', '', 'claimed-nonce', ?, 0, 0, '2026-09-19 11:30:00');
		INSERT INTO cdk_preflight_grants(attempt_nonce, token_hash, created_at) VALUES
			('expired-nonce', 'expired-grant-hash', ?),
			('active-nonce', 'active-grant-hash', ?),
			('stale-nonce', 'stale-grant-hash', ?),
			('recent-nonce', 'recent-grant-hash', ?),
			('claimed-nonce', 'claimed-grant-hash', ?),
			('orphan-nonce', 'orphan-grant-hash', ?)
	`, now.Add(-time.Minute).Unix(),
		now.Add(-time.Minute).Unix(), now.Add(-time.Minute).Unix(),
		now.Add(-time.Minute).Unix(), now.Add(-time.Minute).Unix(),
		now.Add(-time.Minute).Unix(), now.Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}

	if err := purgeAllExpiredCDKBindings(now); err != nil {
		t.Fatal(err)
	}
	expired, err := GetBindingByCDK("PULS-EXPIRED")
	if err != nil {
		t.Fatal(err)
	}
	if expired == nil || expired.SessionPayload != "" || expired.RedemptionToken != "" ||
		expired.AccountEmail != "" || expired.AttemptNonce != "" {
		t.Fatalf("expired binding not scrubbed: %+v", expired)
	}
	if expired.QueryStartedAt == 0 || expired.QueryExpiresAt == 0 {
		t.Fatalf("expiry tombstone was removed: %+v", expired)
	}
	active, err := GetBindingByCDK("PULS-ACTIVE")
	if err != nil {
		t.Fatal(err)
	}
	if active.SessionPayload != "active-session" || active.RedemptionToken != "active-token" ||
		active.AccountEmail != "active@example.com" || active.AttemptNonce != "active-nonce" {
		t.Fatalf("active binding was scrubbed: %+v", active)
	}
	stale, err := GetBindingByCDK("PULS-PREFLIGHT-STALE")
	if err != nil {
		t.Fatal(err)
	}
	if stale.SessionPayload != "" || stale.RedemptionToken != "" ||
		stale.AccountEmail != "" || stale.AttemptNonce != "" {
		t.Fatalf("stale preflight binding not scrubbed: %+v", stale)
	}
	recent, err := GetBindingByCDK("PULS-PREFLIGHT-RECENT")
	if err != nil {
		t.Fatal(err)
	}
	if recent.SessionPayload != "recent-session" || recent.RedemptionToken != "recent-token" ||
		recent.AccountEmail != "recent@example.com" || recent.AttemptNonce != "recent-nonce" {
		t.Fatalf("recent preflight binding was scrubbed: %+v", recent)
	}

	var expiredTask, activeTask, staleTask, recentTask sql.NullString
	if err := database.QueryRow(`SELECT session_json FROM recharge_tasks WHERE task_id='task-PULS-EXPIRED'`).Scan(&expiredTask); err != nil {
		t.Fatal(err)
	}
	if expiredTask.Valid {
		t.Fatalf("expired recharge task retained session: %q", expiredTask.String)
	}
	if err := database.QueryRow(`SELECT session_json FROM recharge_tasks WHERE task_id='task-PULS-ACTIVE'`).Scan(&activeTask); err != nil {
		t.Fatal(err)
	}
	if !activeTask.Valid || activeTask.String != "active-session" {
		t.Fatalf("active recharge task was scrubbed: %+v", activeTask)
	}
	if err := database.QueryRow(`SELECT session_json FROM recharge_tasks WHERE task_id='task-PULS-PREFLIGHT-STALE'`).Scan(&staleTask); err != nil {
		t.Fatal(err)
	}
	if staleTask.Valid {
		t.Fatalf("stale preflight task retained session: %q", staleTask.String)
	}
	if err := database.QueryRow(`SELECT session_json FROM recharge_tasks WHERE task_id='task-PULS-PREFLIGHT-RECENT'`).Scan(&recentTask); err != nil {
		t.Fatal(err)
	}
	if !recentTask.Valid || recentTask.String != "recent-session" {
		t.Fatalf("recent preflight task was scrubbed: %+v", recentTask)
	}
	var remainingGrants int
	var remainingNonce string
	if err := database.QueryRow(`
		SELECT COUNT(*), COALESCE(MIN(attempt_nonce),'') FROM cdk_preflight_grants
	`).Scan(&remainingGrants, &remainingNonce); err != nil {
		t.Fatal(err)
	}
	if remainingGrants != 1 || remainingNonce != "recent-nonce" {
		t.Fatalf("cleanup retained invalid/orphan grants: count=%d nonce=%q", remainingGrants, remainingNonce)
	}
}

func TestPreflightTTLRefreshAndRuntimeCleanup(t *testing.T) {
	database := openCDKQueryTestDB(t)
	createLegacyCDKQueryTables(t, database)
	if err := migrateCDKBindingQueryWindow(); err != nil {
		t.Fatal(err)
	}
	if err := BindCDKAccountEmail("PULS-UNKNOWN-EMAIL-BINDING", "unknown-email-token", "unknown@example.com"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown email binding error=%v, want sql.ErrNoRows", err)
	}
	var unknownCount int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM cdk_session_bindings
		WHERE cdk_code='PULS-UNKNOWN-EMAIL-BINDING'
	`).Scan(&unknownCount); err != nil {
		t.Fatal(err)
	}
	if unknownCount != 0 {
		t.Fatalf("unknown email binding inserted %d rows", unknownCount)
	}

	staleAt := time.Now().UTC().Add(-25 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := database.Exec(`
		INSERT INTO cdk_session_bindings
			(cdk_code, session_payload, redemption_token, account_email, updated_at)
		VALUES ('PULS-REFRESH-TOKEN', 'old-session', 'old-token', 'old@example.com', ?)
	`, staleAt); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
		INSERT INTO recharge_tasks(task_id, cdk_code, session_json)
		VALUES ('refresh-token-old-task', 'PULS-REFRESH-TOKEN', 'old-task-session')
	`); err != nil {
		t.Fatal(err)
	}
	if err := BindCDKRedemptionToken("PULS-REFRESH-TOKEN", "fresh-token"); err != nil {
		t.Fatal(err)
	}
	refreshed, err := GetBindingByCDK("PULS-REFRESH-TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.RedemptionToken != "fresh-token" || refreshed.SessionPayload != "" || refreshed.AccountEmail != "" {
		t.Fatalf("preview did not replace a stale preflight safely: %+v", refreshed)
	}
	if refreshed.QueryStartedAt != 0 || refreshed.QueryExpiresAt != 0 {
		t.Fatalf("preview incorrectly started the seven-day window: %+v", refreshed)
	}
	var oldTaskSession sql.NullString
	if err := database.QueryRow(`
		SELECT session_json FROM recharge_tasks WHERE task_id='refresh-token-old-task'
	`).Scan(&oldTaskSession); err != nil {
		t.Fatal(err)
	}
	if oldTaskSession.Valid {
		t.Fatalf("fresh preview retained old task session: %q", oldTaskSession.String)
	}
	updatedAt, err := time.Parse("2006-01-02 15:04:05", refreshed.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(updatedAt) >= cdkPreflightRetentionTTL {
		t.Fatalf("preview did not refresh the preflight TTL: %s", refreshed.UpdatedAt)
	}

	if _, err := database.Exec(`
		INSERT INTO cdk_session_bindings
			(cdk_code, session_payload, redemption_token, account_email, updated_at)
		VALUES ('PULS-REFRESH-EMAIL', 'old-session', 'old-email-token', 'old@example.com', ?)
	`, staleAt); err != nil {
		t.Fatal(err)
	}
	if err := BindCDKAccountEmail("PULS-REFRESH-EMAIL", "fresh-email-token", "fresh@example.com"); !errors.Is(err, ErrCDKPreflightExpired) {
		t.Fatalf("stale email binding error=%v, want ErrCDKPreflightExpired", err)
	}
	refreshed, err = GetBindingByCDK("PULS-REFRESH-EMAIL")
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.RedemptionToken != "" || refreshed.AccountEmail != "" || refreshed.SessionPayload != "" {
		t.Fatalf("email binding repopulated a stale preflight: %+v", refreshed)
	}
	if refreshed.QueryStartedAt != 0 || refreshed.QueryExpiresAt != 0 {
		t.Fatalf("stale email binding changed the seven-day window: %+v", refreshed)
	}

	// Only a new successful preview may create a fresh preflight grant. Email
	// observed during preflight can enrich that row, but cannot refresh its TTL.
	if err := BindCDKRedemptionToken("PULS-REFRESH-EMAIL", "fresh-email-token"); err != nil {
		t.Fatal(err)
	}
	beforeEmailBind, err := GetBindingByCDK("PULS-REFRESH-EMAIL")
	if err != nil {
		t.Fatal(err)
	}
	if err := BindCDKAccountEmail("PULS-REFRESH-EMAIL", "fresh-email-token", "fresh@example.com"); err != nil {
		t.Fatal(err)
	}
	refreshed, err = GetBindingByCDK("PULS-REFRESH-EMAIL")
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.RedemptionToken != "fresh-email-token" || refreshed.AccountEmail != "fresh@example.com" || refreshed.SessionPayload != "" {
		t.Fatalf("fresh email binding was not saved safely: %+v", refreshed)
	}
	if refreshed.UpdatedAt != beforeEmailBind.UpdatedAt ||
		refreshed.QueryStartedAt != beforeEmailBind.QueryStartedAt ||
		refreshed.QueryExpiresAt != beforeEmailBind.QueryExpiresAt {
		t.Fatalf("email binding refreshed preflight TTL/window: before=%+v after=%+v", beforeEmailBind, refreshed)
	}

	if err := BindCDKSession("PULS-REFRESH-EMAIL", "fresh-email-token", "runtime-session"); err != nil {
		t.Fatal(err)
	}
	if err := BindCDKRedemptionToken("PULS-REFRESH-EMAIL", "repeated-preview-token"); err != nil {
		t.Fatal(err)
	}
	refreshed, err = GetBindingByCDK("PULS-REFRESH-EMAIL")
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.RedemptionToken != "repeated-preview-token" || refreshed.AccountEmail != "" || refreshed.SessionPayload != "" {
		t.Fatalf("new preview token retained prior-attempt identity: %+v", refreshed)
	}
	if refreshed.QueryStartedAt != 0 || refreshed.QueryExpiresAt != 0 {
		t.Fatalf("repeated preview incorrectly started a seven-day window: %+v", refreshed)
	}
	if _, err := database.Exec(`
		UPDATE cdk_session_bindings SET updated_at = ? WHERE cdk_code = 'PULS-REFRESH-EMAIL'
	`, staleAt); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
		INSERT INTO recharge_tasks (task_id, cdk_code, session_json)
		VALUES ('task-runtime-preflight', 'PULS-REFRESH-EMAIL', 'task-session')
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := GetPublicCDKBindingByCode("PULS-REFRESH-EMAIL", time.Now()); !errors.Is(err, ErrCDKQueryNotStarted) {
		t.Fatalf("stale public query error=%v, want ErrCDKQueryNotStarted", err)
	}
	cleared, err := GetBindingByCDK("PULS-REFRESH-EMAIL")
	if err != nil {
		t.Fatal(err)
	}
	if cleared.SessionPayload != "" || cleared.RedemptionToken != "" ||
		cleared.AccountEmail != "" || cleared.AttemptNonce != "" {
		t.Fatalf("public query did not trigger stale cleanup: %+v", cleared)
	}
	var taskSession sql.NullString
	if err := database.QueryRow(`SELECT session_json FROM recharge_tasks WHERE task_id='task-runtime-preflight'`).Scan(&taskSession); err != nil {
		t.Fatal(err)
	}
	if taskSession.Valid {
		t.Fatalf("runtime cleanup retained task session: %q", taskSession.String)
	}
}

func TestRequestCleanupTargetsOnlyCurrentCDK(t *testing.T) {
	database := openCDKQueryTestDB(t)
	createLegacyCDKQueryTables(t, database)
	if err := migrateCDKBindingQueryWindow(); err != nil {
		t.Fatal(err)
	}

	staleAt := time.Now().UTC().Add(-25 * time.Hour).Format("2006-01-02 15:04:05")
	for _, code := range []string{"PULS-TARGET-A", "PULS-TARGET-B"} {
		if _, err := database.Exec(`
			INSERT INTO cdk_session_bindings
				(cdk_code, session_payload, redemption_token, account_email, updated_at)
			VALUES (?, 'old-session', ?, 'old@example.com', ?)
		`, code, "old-token-"+code, staleAt); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`
			INSERT INTO recharge_tasks (task_id, cdk_code, session_json)
			VALUES (?, ?, 'old-task-session')
		`, "task-"+code, code); err != nil {
			t.Fatal(err)
		}
	}

	if err := BindCDKRedemptionToken("PULS-TARGET-A", "fresh-target-token"); err != nil {
		t.Fatal(err)
	}
	a, err := GetBindingByCDK("PULS-TARGET-A")
	if err != nil {
		t.Fatal(err)
	}
	if a.RedemptionToken != "fresh-target-token" || a.SessionPayload != "" || a.AccountEmail != "" {
		t.Fatalf("target binding was not safely refreshed: %+v", a)
	}
	b, err := GetBindingByCDK("PULS-TARGET-B")
	if err != nil {
		t.Fatal(err)
	}
	if b.RedemptionToken != "old-token-PULS-TARGET-B" || b.SessionPayload != "old-session" || b.AccountEmail != "old@example.com" {
		t.Fatalf("unrelated stale binding was touched by request cleanup: %+v", b)
	}
	var taskA, taskB sql.NullString
	if err := database.QueryRow(`SELECT session_json FROM recharge_tasks WHERE task_id='task-PULS-TARGET-A'`).Scan(&taskA); err != nil {
		t.Fatal(err)
	}
	if taskA.Valid {
		t.Fatalf("target task session retained: %q", taskA.String)
	}
	if err := database.QueryRow(`SELECT session_json FROM recharge_tasks WHERE task_id='task-PULS-TARGET-B'`).Scan(&taskB); err != nil {
		t.Fatal(err)
	}
	if !taskB.Valid || taskB.String != "old-task-session" {
		t.Fatalf("unrelated task was touched: %+v", taskB)
	}
}

func TestActivateCDKQueryWindowRejectsStalePreflightAtExactBoundary(t *testing.T) {
	database := openCDKQueryTestDB(t)
	createLegacyCDKQueryTables(t, database)
	if err := migrateCDKBindingQueryWindow(); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	insert := func(code, token string, updatedAt time.Time) {
		t.Helper()
		if _, err := database.Exec(`
			INSERT INTO cdk_session_bindings
				(cdk_code, session_payload, redemption_token, account_email, updated_at)
			VALUES (?, 'session-secret', ?, 'customer@example.com', ?)
		`, code, token, updatedAt.UTC().Format("2006-01-02 15:04:05")); err != nil {
			t.Fatal(err)
		}
	}

	insert("PULS-ACTIVATE-STALE", "activate-stale-token", now.Add(-24*time.Hour))
	if err := ActivateCDKQueryWindowByToken("activate-stale-token", now); !errors.Is(err, ErrCDKPreflightExpired) {
		t.Fatalf("exact 24-hour activation error=%v, want ErrCDKPreflightExpired", err)
	}
	stale, err := GetBindingByCDK("PULS-ACTIVATE-STALE")
	if err != nil {
		t.Fatal(err)
	}
	if stale.QueryStartedAt != 0 || stale.QueryExpiresAt != 0 {
		t.Fatalf("stale token activated a seven-day window: %+v", stale)
	}
	if stale.RedemptionToken != "" || stale.SessionPayload != "" ||
		stale.AccountEmail != "" || stale.AttemptNonce != "" {
		t.Fatalf("stale activation credentials were retained: %+v", stale)
	}

	insert("PULS-ACTIVATE-FRESH", "activate-fresh-token", now.Add(-24*time.Hour+time.Second))
	if err := ActivateCDKQueryWindowByToken("activate-fresh-token", now); err != nil {
		t.Fatalf("23:59:59 preflight was rejected: %v", err)
	}
	fresh, err := GetBindingByCDK("PULS-ACTIVATE-FRESH")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.QueryStartedAt != now.Unix() || fresh.QueryExpiresAt != now.Add(cdkPublicQueryWindow).Unix() {
		t.Fatalf("fresh preflight window=%+v", fresh)
	}
}

func TestActivateRejectsExpiredActiveWindowWithoutPriorQuery(t *testing.T) {
	database := openCDKQueryTestDB(t)
	createLegacyCDKQueryTables(t, database)
	if err := migrateCDKBindingQueryWindow(); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	expiresAt := startedAt.Add(cdkPublicQueryWindow)
	const code = "PULS-ACTIVE-EXPIRY"
	const token = "active-expiry-token"
	if _, err := database.Exec(`
		INSERT INTO cdk_session_bindings
			(cdk_code, session_payload, redemption_token, account_email,
			 query_started_at, query_expires_at, updated_at)
		VALUES (?, 'session-secret', ?, 'customer@example.com', ?, ?, ?)
	`, code, token, startedAt.Unix(), expiresAt.Unix(), startedAt.UTC().Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
		INSERT INTO recharge_tasks (task_id, cdk_code, session_json)
		VALUES ('task-active-expiry', ?, 'task-session-secret')
	`, code); err != nil {
		t.Fatal(err)
	}

	// One second before expiry may retry, but the original window must not move.
	if err := ActivateCDKQueryWindowByToken(token, expiresAt.Add(-time.Second)); err != nil {
		t.Fatalf("last active second was rejected: %v", err)
	}
	active, err := GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if active.QueryStartedAt != startedAt.Unix() || active.QueryExpiresAt != expiresAt.Unix() {
		t.Fatalf("retry extended the original window: %+v", active)
	}

	// No Get/Public purge occurs before this call: activation itself must reject
	// the exact seven-day boundary and scrub the retained credentials.
	if err := ActivateCDKQueryWindowByToken(token, expiresAt); !errors.Is(err, ErrCDKQueryExpired) {
		t.Fatalf("exact seven-day activation error=%v, want ErrCDKQueryExpired", err)
	}
	expired, err := GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if expired.QueryStartedAt != startedAt.Unix() || expired.QueryExpiresAt != expiresAt.Unix() {
		t.Fatalf("expired window tombstone changed: %+v", expired)
	}
	if expired.RedemptionToken != "" || expired.SessionPayload != "" ||
		expired.AccountEmail != "" || expired.AttemptNonce != "" {
		t.Fatalf("expired active credentials were retained: %+v", expired)
	}
	var taskSession sql.NullString
	if err := database.QueryRow(`SELECT session_json FROM recharge_tasks WHERE task_id='task-active-expiry'`).Scan(&taskSession); err != nil {
		t.Fatal(err)
	}
	if taskSession.Valid {
		t.Fatalf("expired active task session retained: %q", taskSession.String)
	}
}

func TestExpiredSnapshotCleanupDoesNotPurgeInterleavedFreshPreview(t *testing.T) {
	database := openCDKQueryTestDB(t)
	createLegacyCDKQueryTables(t, database)
	if err := migrateCDKBindingQueryWindow(); err != nil {
		t.Fatal(err)
	}

	const code = "PULS-EXPIRED-PREVIEW-RACE"
	const oldToken = "expired-preview-token"
	const oldNonce = "expired-preview-nonce"
	const freshToken = "fresh-preview-token"
	const freshGrant = "fresh-preview-grant"
	now := time.Now().UTC()
	startedAt := now.Add(-cdkPublicQueryWindow - time.Minute)
	expiresAt := startedAt.Add(cdkPublicQueryWindow)
	if _, err := database.Exec(`
		INSERT INTO cdk_session_bindings
			(cdk_code, session_payload, redemption_token, account_email, attempt_nonce,
			 submit_claimed_at, query_started_at, query_expires_at, updated_at)
		VALUES (?, 'expired-session', ?, 'expired@example.com', ?, ?, ?, ?, ?)
	`, code, oldToken, oldNonce, startedAt.Unix(), startedAt.Unix(), expiresAt.Unix(),
		startedAt.Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}

	// Model the deterministic race ordering: an expiry request reads the old
	// attempt, then a successful upstream Preview replaces it before the stale
	// request reaches credential cleanup.
	expiredSnapshot, err := GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	freshNonce, err := BindCDKRedemptionTokenCASWithAttempt(code, freshToken, expiredSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterCDKPreflightGrant(code, freshToken, freshNonce, freshGrant, now); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
		INSERT INTO recharge_tasks (task_id, cdk_code, session_json)
		VALUES ('task-expired-preview-race', ?, 'fresh-task-session')
	`, code); err != nil {
		t.Fatal(err)
	}

	if err := purgeExpiredCDKBindingSnapshot(expiredSnapshot, now); err != nil {
		t.Fatal(err)
	}
	fresh, err := GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.RedemptionToken != freshToken || fresh.AttemptNonce != freshNonce {
		t.Fatalf("stale expiry cleanup removed fresh Preview identity: %+v", fresh)
	}
	if fresh.QueryStartedAt != 0 || fresh.QueryExpiresAt != 0 || fresh.SubmitClaimedAt != 0 {
		t.Fatalf("fresh Preview window was changed by stale cleanup: %+v", fresh)
	}
	var grantCount int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM cdk_preflight_grants
		WHERE attempt_nonce = ? AND token_hash = ?
	`, freshNonce, legacySHA256(freshGrant)).Scan(&grantCount); err != nil {
		t.Fatal(err)
	}
	if grantCount != 1 {
		t.Fatalf("stale expiry cleanup removed fresh Preview grant: count=%d", grantCount)
	}
	var taskSession sql.NullString
	if err := database.QueryRow(`
		SELECT session_json FROM recharge_tasks WHERE task_id='task-expired-preview-race'
	`).Scan(&taskSession); err != nil {
		t.Fatal(err)
	}
	if !taskSession.Valid || taskSession.String != "fresh-task-session" {
		t.Fatalf("stale expiry cleanup removed fresh attempt task session: %+v", taskSession)
	}
}

func TestConcurrentTargetedBindingUpdatesAvoidBusyErrors(t *testing.T) {
	database := openCDKQueryTestDB(t)
	createLegacyCDKQueryTables(t, database)
	if err := migrateCDKBindingQueryWindow(); err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(16)

	const count = 80
	for i := 0; i < count; i++ {
		code := fmt.Sprintf("PULS-CONCURRENT-%03d", i)
		if _, err := database.Exec(`
			INSERT INTO cdk_session_bindings (cdk_code, session_payload, redemption_token, updated_at)
			VALUES (?, '', ?, CURRENT_TIMESTAMP)
		`, code, "initial-"+code); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, count*2)
	for i := 0; i < count; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			code := fmt.Sprintf("PULS-CONCURRENT-%03d", i)
			token := "fresh-" + code
			if err := BindCDKRedemptionToken(code, token); err != nil {
				errs <- err
				return
			}
			if err := BindCDKAccountEmail(code, token, fmt.Sprintf("user%03d@example.com", i)); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent binding update: %v", err)
	}
	if t.Failed() {
		return
	}

	var rows int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM cdk_session_bindings
		WHERE query_started_at = 0 AND query_expires_at = 0
		  AND TRIM(redemption_token) != '' AND TRIM(account_email) != ''
	`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != count {
		t.Fatalf("fully updated rows=%d, want %d", rows, count)
	}
}
