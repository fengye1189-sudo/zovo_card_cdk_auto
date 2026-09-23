package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func insertPublicLocalCDK(t *testing.T, code, status, email string, createdAt int64) int64 {
	t.Helper()
	result, err := db.DB.Exec(`
		INSERT INTO local_cdks
			(code_hash, prefix, plan, status, expires_at, created_at, email, message)
		VALUES (?, ?, 'plus', ?, ?, ?, ?, 'internal operator note')
	`, localHash(normalizePublicCDK(code)), "PULS-TEST", status,
		createdAt+30*86400, createdAt, email)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestLocalPublicLookupUnsubmittedIsUnusedWithoutIdentity(t *testing.T) {
	newLocalFixture(t)
	const (
		code  = "PULS-UNSUBMITTED-TEST"
		email = "private.customer@example.com"
	)
	insertPublicLocalCDK(t, code, "unused", email, time.Now().Unix())

	got := lookupOneCDK(context.Background(), code, "test-device")
	if got.Status != "unused" || got.Used || !got.CanResubmit {
		t.Fatalf("unsubmitted lookup = %+v", got)
	}
	if got.AccountEmail != "" {
		t.Fatalf("unsubmitted lookup leaked account email: %q", got.AccountEmail)
	}
	if got.Notes != "" || strings.Contains(got.Message, "operator") {
		t.Fatalf("unsubmitted lookup leaked internal data: %+v", got)
	}
}

func TestLocalLookupFailsClosedWithoutImmutableSubmissionTime(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     string
		expired    bool
		wantStatus string
	}{
		{name: "used is sensitive", status: "consumed", wantStatus: "query_expired"},
		{name: "processing is sensitive", status: "reserved", wantStatus: "query_expired"},
		{name: "failed is sensitive", status: "failed", wantStatus: "query_expired"},
		{name: "unused is basic", status: "unused", wantStatus: "unused"},
		{name: "disabled is basic", status: "disabled", wantStatus: "disabled"},
		{name: "expired is basic", status: "unused", expired: true, wantStatus: "expired"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newLocalFixture(t)
			code := f.code(t)
			expiresAt := time.Now().Add(24 * time.Hour).Unix()
			if test.expired {
				expiresAt = time.Now().Add(-time.Hour).Unix()
			}
			if _, err := db.DB.Exec(`
				UPDATE local_cdks
				SET status=?, expires_at=?, email='private.lookup@example.com',
				    message='INTERNAL token=lookup-secret card=424242'
				WHERE id=1
			`, test.status, expiresAt); err != nil {
				t.Fatal(err)
			}

			got := lookupOneCDK(context.Background(), code, "test-device")
			if got.Status != test.wantStatus {
				t.Fatalf("lookup status=%q result=%+v, want %q", got.Status, got, test.wantStatus)
			}
			if got.AccountEmail != "" || got.Notes != "" {
				t.Fatalf("basic/fail-closed lookup leaked identity: %+v", got)
			}
			encoded, _ := json.Marshal(got)
			if strings.Contains(string(encoded), "lookup-secret") || strings.Contains(string(encoded), "424242") {
				t.Fatalf("lookup leaked internal fields: %s", encoded)
			}
		})
	}
}

func createLegacyLookupTables(t *testing.T) {
	t.Helper()
	for _, statement := range []string{
		`CREATE TABLE cd_keys (
			code TEXT PRIMARY KEY, plan_type TEXT, status TEXT,
			used_at DATETIME, expires_at DATETIME
		)`,
		`CREATE TABLE cardplatform_cdk_codes (
			code TEXT, status TEXT, plan TEXT, created_at DATETIME
		)`,
		`CREATE TABLE recharge_tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			cdk_code TEXT, session_json TEXT, task_status TEXT, account_email TEXT,
			completed_at DATETIME, notes TEXT, created_at DATETIME
		)`,
		`CREATE TABLE cdk_session_bindings (
			cdk_code TEXT PRIMARY KEY, redemption_token TEXT,
			session_payload TEXT NOT NULL DEFAULT '', account_email TEXT NOT NULL DEFAULT '',
			attempt_nonce TEXT NOT NULL DEFAULT '',
			submit_claimed_at INTEGER NOT NULL DEFAULT 0,
			query_started_at INTEGER NOT NULL DEFAULT 0,
			query_expires_at INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE cdk_preflight_grants (
			attempt_nonce TEXT NOT NULL,
			token_hash TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY(attempt_nonce, token_hash)
		)`,
	} {
		if _, err := db.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLegacyLookupNeedsTimestampOrActiveBindingForSensitiveStatus(t *testing.T) {
	for _, test := range []struct {
		name       string
		cpStatus   string
		taskStatus string
		binding    bool
		wantStatus string
	}{
		{name: "used without timestamp", cpStatus: "used", wantStatus: "query_expired"},
		{name: "processing without timestamp", cpStatus: "unused", taskStatus: "processing", wantStatus: "query_expired"},
		{name: "failed without timestamp", cpStatus: "unused", taskStatus: "failed", wantStatus: "query_expired"},
		{name: "active binding shadows legacy used status", cpStatus: "used", binding: true, wantStatus: "processing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			newLocalFixture(t)
			createLegacyLookupTables(t)
			code := "SXC-LEGACY-" + strings.ToUpper(strings.ReplaceAll(test.name, " ", "-"))
			if _, err := db.DB.Exec(`
				INSERT INTO cardplatform_cdk_codes(code, status, plan, created_at)
				VALUES (?, ?, 'plus', CURRENT_TIMESTAMP)
			`, code, test.cpStatus); err != nil {
				t.Fatal(err)
			}
			if test.taskStatus != "" {
				if _, err := db.DB.Exec(`
					INSERT INTO recharge_tasks
						(cdk_code, task_status, account_email, notes, created_at)
					VALUES (?, ?, 'private.legacy@example.com', 'INTERNAL legacy secret', NULL)
				`, code, test.taskStatus); err != nil {
					t.Fatal(err)
				}
			}
			if test.binding {
				now := time.Now().Unix()
				if _, err := db.DB.Exec(`
					INSERT INTO cdk_session_bindings
						(cdk_code, redemption_token, query_started_at, query_expires_at)
					VALUES (?, '', ?, ?)
				`, code, now-60, now+3600); err != nil {
					t.Fatal(err)
				}
			}

			got := lookupOneCDK(context.Background(), code, "test-device")
			if got.Status != test.wantStatus {
				t.Fatalf("legacy lookup status=%q result=%+v, want %q", got.Status, got, test.wantStatus)
			}
			if got.Status == "query_expired" && got.AccountEmail != "" {
				t.Fatalf("fail-closed legacy lookup leaked email: %+v", got)
			}
		})
	}
}

func TestLegacyLookupUsesEarliestTaskAsImmutableWindowStart(t *testing.T) {
	newLocalFixture(t)
	createLegacyLookupTables(t)
	const code = "SXC-EARLIEST-TASK-WINDOW"
	oldCreatedAt := time.Now().UTC().Add(-8 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	recentCreatedAt := time.Now().UTC().Format("2006-01-02 15:04:05")
	if _, err := db.DB.Exec(`
		INSERT INTO cardplatform_cdk_codes(code, status, plan, created_at)
		VALUES (?, 'used', 'plus', ?);
		INSERT INTO recharge_tasks
			(cdk_code, task_status, account_email, notes, created_at)
		VALUES (?, 'processing', 'first.private@example.com', 'old internal note', ?);
		INSERT INTO recharge_tasks
			(cdk_code, task_status, account_email, notes, created_at)
		VALUES (?, 'completed', 'first.private@example.com', 'new internal note', ?)
	`, code, oldCreatedAt, code, oldCreatedAt, code, recentCreatedAt); err != nil {
		t.Fatal(err)
	}

	got := lookupOneCDK(context.Background(), code, "test-device")
	if got.Status != "query_expired" {
		t.Fatalf("later task moved immutable window: %+v", got)
	}
	encoded, _ := json.Marshal(got)
	text := string(encoded)
	for _, forbidden := range []string{
		"first.private@example.com",
		"old internal note", "new internal note",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("expired multi-task lookup leaked %q: %s", forbidden, text)
		}
	}
}

func TestLegacyLookupFailsClosedOnConflictingTaskEmails(t *testing.T) {
	newLocalFixture(t)
	createLegacyLookupTables(t)
	const code = "SXC-LEGACY-LOOKUP-CONFLICTING-EMAILS"
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := db.DB.Exec(`
		INSERT INTO recharge_tasks(cdk_code, task_status, account_email, notes, created_at)
		VALUES (?, 'processing', 'first.private@example.com', 'first internal note', ?);
		INSERT INTO recharge_tasks(cdk_code, task_status, account_email, notes, created_at)
		VALUES (?, 'completed', 'second.private@example.com', 'second internal note', ?)
	`, code, now.Add(-time.Hour).Format("2006-01-02 15:04:05"),
		code, now.Add(-time.Minute).Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}

	got := lookupOneCDK(context.Background(), code, "test-device")
	if got.Status != "unknown" || got.AccountEmail != "" || got.Plan != "" || got.UsedAt != nil ||
		got.Used || got.CanResubmit {
		t.Fatalf("conflicting legacy identities were not closed: %+v", got)
	}
	encoded, _ := json.Marshal(got)
	for _, forbidden := range []string{
		"first.private@example.com", "second.private@example.com", "internal note",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("conflicting legacy lookup leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestLegacyLookupUsesOnlyUniqueTaskEmailWhenLatestIsEmpty(t *testing.T) {
	newLocalFixture(t)
	createLegacyLookupTables(t)
	const (
		code  = "SXC-LEGACY-LOOKUP-UNIQUE-EMAIL"
		email = "unique.private@example.com"
	)
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := db.DB.Exec(`
		INSERT INTO recharge_tasks(cdk_code, task_status, account_email, notes, created_at)
		VALUES (?, 'processing', ?, 'first internal note', ?);
		INSERT INTO recharge_tasks(cdk_code, task_status, account_email, notes, created_at)
		VALUES (?, 'completed', '', 'latest internal note', ?)
	`, code, email, now.Add(-time.Hour).Format("2006-01-02 15:04:05"),
		code, now.Add(-time.Minute).Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}

	got := lookupOneCDK(context.Background(), code, "test-device")
	if got.Status != "used" || !got.Used || got.AccountEmail != maskEmail(email) {
		t.Fatalf("unique legacy identity was not selected safely: %+v", got)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), email) || strings.Contains(string(encoded), "internal note") {
		t.Fatalf("unique legacy lookup leaked raw identity or notes: %s", encoded)
	}
}

func TestFreshPendingBindingShadowsLegacyTaskAndBillingIdentity(t *testing.T) {
	newLocalFixture(t)
	createLegacyLookupTables(t)
	const (
		code     = "SXC-FRESH-PENDING-SHADOWS-OLD"
		newToken = "fresh-pending-shadow-token"
		oldEmail = "old.attempt.private@example.com"
	)
	oldCreated := time.Now().UTC().Add(-30 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := db.DB.Exec(`
		INSERT INTO cardplatform_cdk_codes(code, status, plan, created_at)
		VALUES (?, 'active', 'plus', ?);
		INSERT INTO recharge_tasks
			(cdk_code, session_json, task_status, account_email, notes, created_at)
		VALUES (?, 'old-session-secret', 'completed', ?, 'old internal note', ?)
	`, code, oldCreated, code, oldEmail, oldCreated); err != nil {
		t.Fatal(err)
	}
	if err := db.BindCDKRedemptionToken(code, newToken); err != nil {
		t.Fatal(err)
	}

	got := lookupOneCDK(context.Background(), code, "test-device")
	if got.Status != "unused" || got.Used || !got.CanResubmit {
		t.Fatalf("fresh pending lookup=%+v", got)
	}
	encoded, _ := json.Marshal(got)
	for _, forbidden := range []string{oldEmail, "old internal note", "old-session-secret"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("fresh pending lookup leaked %q: %s", forbidden, encoded)
		}
	}

	var accountHubCalls atomic.Int32
	accountHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accountHubCalls.Add(1)
		_ = json.NewEncoder(w).Encode(gin.H{"invoices": []gin.H{}})
	}))
	defer accountHub.Close()
	t.Setenv("ACCOUNTHUB_BASE_URL", accountHub.URL)
	router := gin.New()
	router.POST("/billing", SessionBillingCheck)
	status, response := postBillingCheck(t, router, code)
	if status != http.StatusNotFound || response["status"] != "not_submitted" {
		t.Fatalf("fresh pending billing status=%d response=%v", status, response)
	}
	if accountHubCalls.Load() != 0 {
		t.Fatalf("fresh pending billing called accounthub %d times", accountHubCalls.Load())
	}
	encoded, _ = json.Marshal(response)
	if strings.Contains(string(encoded), oldEmail) || strings.Contains(string(encoded), "old internal note") {
		t.Fatalf("fresh pending billing leaked legacy identity: %s", encoded)
	}
}

func TestActiveBindingIgnoresAllRechargeTasks(t *testing.T) {
	newLocalFixture(t)
	createLegacyLookupTables(t)
	const (
		code               = "SXC-ACTIVE-IGNORES-TASKS"
		token              = "active-current-attempt-token"
		oldEmail           = "old.task@old-leak.invalid"
		lateEmail          = "late.task@late-leak.invalid"
		bindingEmail       = "binding@binding-leak.invalid"
		currentResultEmail = "current.token@current-result.example"
	)
	oldCreated := time.Now().UTC().Add(-30 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := db.DB.Exec(`
		INSERT INTO cardplatform_cdk_codes(code, status, plan, created_at)
		VALUES (?, 'active', 'plus', ?);
		INSERT INTO recharge_tasks
			(cdk_code, session_json, task_status, account_email, notes, created_at)
		VALUES (?, 'old-task-session', 'completed', ?, 'old task note', ?)
	`, code, oldCreated, code, oldEmail, oldCreated); err != nil {
		t.Fatal(err)
	}
	if err := db.BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := db.ActivateCDKQueryWindowByToken(token, startedAt); err != nil {
		t.Fatal(err)
	}
	if err := db.BindCDKAccountEmail(code, token, bindingEmail); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`
		INSERT INTO recharge_tasks
			(cdk_code, session_json, task_status, account_email, notes, created_at)
		VALUES (?, 'late-task-session', 'completed', ?, 'late task note', CURRENT_TIMESTAMP)
	`, code, lateEmail); err != nil {
		t.Fatal(err)
	}

	resultUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("token"); got != token {
			t.Errorf("result token=%q, want current attempt token", got)
		}
		_ = json.NewEncoder(w).Encode(gin.H{
			"status": "success",
			"data": gin.H{"order": gin.H{
				"status":        "processing",
				"account_email": currentResultEmail,
			}},
		})
	}))
	defer resultUpstream.Close()
	t.Setenv("CARD_API_BASE", resultUpstream.URL)
	got := lookupOneCDK(context.Background(), code, "test-device")
	if got.Status != "processing" || got.Used {
		t.Fatalf("active task-isolation lookup=%+v", got)
	}
	if got.AccountEmail == "" || !strings.Contains(got.AccountEmail, "@current-result.example") {
		t.Fatalf("active task-isolation lookup selected wrong identity: %+v", got)
	}
	encoded, _ := json.Marshal(got)
	for _, forbidden := range []string{
		oldEmail, lateEmail, bindingEmail,
		"old task note", "late task note", "old-task-session", "late-task-session",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("active task-isolation lookup leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestActiveLookupRevalidatesAttemptAfterResultNetworkRoundTrip(t *testing.T) {
	newLocalFixture(t)
	createLegacyLookupTables(t)
	const (
		code             = "SXC-LOOKUP-RESULT-NONCE-ABA"
		token            = "lookup-provider-reused-token"
		oldResponseEmail = "old.result@must-not-leak.invalid"
		newAttemptEmail  = "new.attempt@must-not-leak.invalid"
	)
	if err := db.BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateCDKQueryWindowByToken(token, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	oldAttempt, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	resultUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("token"); got != token {
			t.Errorf("result token=%q, want %q", got, token)
		}
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(gin.H{
			"status": "success",
			"data": gin.H{"order": gin.H{
				"status":        "processing",
				"account_email": oldResponseEmail,
			}},
		})
	}))
	defer resultUpstream.Close()
	t.Setenv("CARD_API_BASE", resultUpstream.URL)

	lookupCh := make(chan cdkLookupResult, 1)
	go func() {
		lookupCh <- lookupOneCDK(context.Background(), code, "lookup-aba-device")
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("lookup did not reach upstream result")
	}

	pastStart := time.Now().UTC().Add(-8 * 24 * time.Hour).Unix()
	if _, err := db.DB.Exec(`
		UPDATE cdk_session_bindings
		SET query_started_at=?, query_expires_at=? WHERE cdk_code=?
	`, pastStart, pastStart+int64(7*24*time.Hour/time.Second), code); err != nil {
		close(release)
		t.Fatal(err)
	}
	expired, err := db.GetBindingByCDK(code)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	if err := db.BindCDKRedemptionTokenCAS(code, token, expired); err != nil {
		close(release)
		t.Fatal(err)
	}
	if err := db.ActivateCDKQueryWindowByToken(token, time.Now()); err != nil {
		close(release)
		t.Fatal(err)
	}
	if err := db.BindCDKAccountEmail(code, token, newAttemptEmail); err != nil {
		close(release)
		t.Fatal(err)
	}
	current, err := db.GetBindingByCDK(code)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	if current.AttemptNonce == oldAttempt.AttemptNonce {
		close(release)
		t.Fatal("same provider token did not rotate lookup attempt identity")
	}

	close(release)
	var got cdkLookupResult
	select {
	case got = <-lookupCh:
	case <-time.After(3 * time.Second):
		t.Fatal("lookup did not finish after upstream release")
	}
	if got.Status != "unknown" || got.AccountEmail != "" || got.Used || got.CanResubmit {
		t.Fatalf("stale in-flight lookup did not fail closed: %+v", got)
	}
	encoded, _ := json.Marshal(got)
	for _, secret := range []string{oldResponseEmail, newAttemptEmail} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("stale in-flight lookup leaked %q: %s", secret, encoded)
		}
	}
}

func TestFailedCurrentAttemptCanResubmitWhenCardPlatformIsReusable(t *testing.T) {
	for _, cardStatus := range []string{"active", "unused"} {
		t.Run(cardStatus, func(t *testing.T) {
			newLocalFixture(t)
			createLegacyLookupTables(t)
			code := "SXC-FAILED-REUSABLE-" + strings.ToUpper(cardStatus)
			token := "failed-reusable-token-" + cardStatus
			if _, err := db.DB.Exec(`
				INSERT INTO cardplatform_cdk_codes(code, status, plan, created_at)
				VALUES (?, ?, 'plus', CURRENT_TIMESTAMP)
			`, code, cardStatus); err != nil {
				t.Fatal(err)
			}
			if err := db.BindCDKRedemptionToken(code, token); err != nil {
				t.Fatal(err)
			}
			if err := db.ActivateCDKQueryWindowByToken(token, time.Now().Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			if _, err := db.DB.Exec(`
				INSERT INTO recharge_tasks
					(cdk_code, task_status, account_email, notes, created_at)
				VALUES (?, 'failed', 'failed.customer@example.com',
				        'INTERNAL provider failure secret', CURRENT_TIMESTAMP)
			`, code); err != nil {
				t.Fatal(err)
			}

			resultUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Query().Get("token"); got != token {
					t.Errorf("result token=%q, want %q", got, token)
				}
				_ = json.NewEncoder(w).Encode(gin.H{
					"status":       "success",
					"can_resubmit": false,
					"data": gin.H{"order": gin.H{
						"status":       "failed",
						"can_resubmit": true,
						"message":      "UPSTREAM private failure detail",
					}},
				})
			}))
			defer resultUpstream.Close()
			t.Setenv("CARD_API_BASE", resultUpstream.URL)
			got := lookupOneCDK(context.Background(), code, "test-device")
			if got.Status != "failed" || got.Used || !got.CanResubmit {
				t.Fatalf("failed reusable lookup=%+v", got)
			}
			encoded, _ := json.Marshal(got)
			if strings.Contains(string(encoded), "provider failure secret") ||
				strings.Contains(string(encoded), "UPSTREAM private failure") ||
				strings.Contains(string(encoded), "INTERNAL") {
				t.Fatalf("failed reusable lookup leaked internal note: %s", encoded)
			}
		})
	}
}

func TestFailedActiveAttemptWithoutExplicitRetryPermissionRemainsLocked(t *testing.T) {
	for _, cardStatus := range []string{"active", "unused"} {
		for _, permission := range []string{"missing", "false"} {
			t.Run(cardStatus+"/"+permission, func(t *testing.T) {
				newLocalFixture(t)
				createLegacyLookupTables(t)
				code := "SXC-FAILED-LOCKED-" + strings.ToUpper(cardStatus+"-"+permission)
				token := "failed-locked-token-" + cardStatus + "-" + permission
				if _, err := db.DB.Exec(`
					INSERT INTO cardplatform_cdk_codes(code, status, plan, created_at)
					VALUES (?, ?, 'plus', CURRENT_TIMESTAMP)
				`, code, cardStatus); err != nil {
					t.Fatal(err)
				}
				if err := db.BindCDKRedemptionToken(code, token); err != nil {
					t.Fatal(err)
				}
				if err := db.ActivateCDKQueryWindowByToken(token, time.Now().Add(-time.Minute)); err != nil {
					t.Fatal(err)
				}
				resultUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					order := gin.H{"status": "failed"}
					if permission == "false" {
						order["can_resubmit"] = false
					}
					payload := gin.H{
						"status": "success",
						"data":   gin.H{"order": order},
					}
					if permission == "false" {
						payload["can_resubmit"] = true
					}
					_ = json.NewEncoder(w).Encode(payload)
				}))
				defer resultUpstream.Close()
				t.Setenv("CARD_API_BASE", resultUpstream.URL)

				got := lookupOneCDK(context.Background(), code, "test-device")
				if got.Status != "failed" || got.Used || got.CanResubmit {
					t.Fatalf("inventory status %q granted retry without explicit current-attempt permission: %+v", cardStatus, got)
				}
			})
		}
	}
}

func TestLookupFailureDoesNotExposeStoredNotes(t *testing.T) {
	newLocalFixture(t)
	createLegacyLookupTables(t)
	const code = "SXC-FAILED-NOTES-TEST"
	if _, err := db.DB.Exec(`
		INSERT INTO cd_keys(code, plan_type, status) VALUES (?, 'plus', 'active');
		INSERT INTO recharge_tasks(cdk_code, task_status, account_email, notes, created_at)
		VALUES (?, 'failed', 'failure@example.com',
		        'INTERNAL provider card=424242 token=do-not-leak', CURRENT_TIMESTAMP)
	`, code, code); err != nil {
		t.Fatal(err)
	}

	got := lookupOneCDK(context.Background(), code, "test-device")
	if got.Status != "failed" || !got.CanResubmit {
		t.Fatalf("failed lookup = %+v", got)
	}
	encoded, _ := json.Marshal(got)
	text := string(encoded)
	for _, forbidden := range []string{"INTERNAL", "424242", "do-not-leak"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("failed lookup leaked %q: %s", forbidden, text)
		}
	}
}

func TestLocalPublicQueryWindowLastSecondAndBoundary(t *testing.T) {
	newLocalFixture(t)
	const code = "PULS-BOUNDARY-LOCAL"
	submittedAt := time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC).Unix()
	localID := insertPublicLocalCDK(t, code, "consumed", "boundary@example.com", submittedAt-60)
	if _, err := db.DB.Exec(`
		INSERT INTO local_card_selections(local_id, card_id, selected_at)
		VALUES (?, 123, ?)
	`, localID, submittedAt); err != nil {
		t.Fatal(err)
	}

	lastSecond := time.Unix(submittedAt+int64(publicCDKQueryWindow/time.Second)-1, 0)
	active, err := loadLocalPublicCDK(code, lastSecond)
	if err != nil {
		t.Fatalf("6d23:59:59 should remain queryable: %v", err)
	}
	if active == nil || active.Email != "boundary@example.com" {
		t.Fatalf("active local query = %+v", active)
	}

	exactBoundary := time.Unix(submittedAt+int64(publicCDKQueryWindow/time.Second), 0)
	expired, err := loadLocalPublicCDK(code, exactBoundary)
	if err != errLocalCDKQueryExpired {
		t.Fatalf("exact seven-day boundary err=%v, want %v", err, errLocalCDKQueryExpired)
	}
	if expired == nil || expired.Email != "" {
		t.Fatalf("expired local query retained public identity: %+v", expired)
	}

	// The public status response uses a stable, privacy-safe terminal state at
	// that same boundary (rather than returning an internal database error).
	if _, err := db.DB.Exec(`
		UPDATE local_card_selections SET selected_at=? WHERE local_id=?
	`, time.Now().Add(-publicCDKQueryWindow).Unix(), localID); err != nil {
		t.Fatal(err)
	}
	public := lookupOneCDK(context.Background(), code, "test-device")
	if public.Status != "query_expired" || public.AccountEmail != "" {
		t.Fatalf("expired public status = %+v", public)
	}
}

func postBillingCheck(t *testing.T, router http.Handler, code string) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(gin.H{"cdk_code": code})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/billing", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode billing response (%d): %v; body=%s", recorder.Code, err, recorder.Body.String())
	}
	return recorder.Code, response
}

func TestLocalBillingByCDKHonorsSubmissionWindow(t *testing.T) {
	newLocalFixture(t)
	const (
		code  = "PULS-LOCAL-BILLING"
		email = "billing.customer@example.com"
	)
	submittedAt := time.Now().Add(-time.Hour).Unix()
	localID := insertPublicLocalCDK(t, code, "consumed", email, submittedAt-60)
	router := gin.New()
	router.POST("/billing", SessionBillingCheck)

	status, response := postBillingCheck(t, router, code)
	errorText, _ := response["error"].(string)
	if status != http.StatusNotFound || !strings.Contains(errorText, "尚未提交") {
		t.Fatalf("unsubmitted billing status=%d response=%v", status, response)
	}

	if _, err := db.DB.Exec(`
		INSERT INTO local_card_selections(local_id, card_id, selected_at)
		VALUES (?, 456, ?)
	`, localID, submittedAt); err != nil {
		t.Fatal(err)
	}

	var queriedEmail string
	accountHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queriedEmail = r.URL.Query().Get("email")
		_ = json.NewEncoder(w).Encode(gin.H{
			"invoice_url": "https://secret.example/account-history",
			"invoices": []gin.H{
				{
					"number":             "inv-this-redemption",
					"status":             "paid",
					"created":            submittedAt + 30,
					"amount_paid":        2000,
					"hosted_invoice_url": "https://secret.example/invoice",
					"invoice_pdf":        "https://secret.example/pdf",
					"customer_email":     email,
				},
			},
		})
	}))
	defer accountHub.Close()
	t.Setenv("ACCOUNTHUB_BASE_URL", accountHub.URL)

	status, response = postBillingCheck(t, router, code)
	if status != http.StatusOK {
		t.Fatalf("billing status=%d response=%v", status, response)
	}
	if queriedEmail != email {
		t.Fatalf("accounthub email=%q, want %q", queriedEmail, email)
	}
	if response["auth_source"] != "cdk" {
		t.Fatalf("auth_source=%v", response["auth_source"])
	}
	summary, ok := response["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary=%T %v", response["summary"], response["summary"])
	}
	if got := summary["email"]; got == email || got == "" {
		t.Fatalf("billing summary email was not masked: %v", got)
	}
	invoices, ok := response["invoices"].([]any)
	if !ok || len(invoices) != 1 {
		t.Fatalf("invoices=%T %v", response["invoices"], response["invoices"])
	}
	encoded, _ := json.Marshal(invoices[0])
	if strings.Contains(string(encoded), "secret.example") || strings.Contains(string(encoded), email) {
		t.Fatalf("billing response leaked URL or raw identity: %s", encoded)
	}

	// The exact seven-day boundary is expired. Moving this same immutable
	// submission into the past must deny access without calling accounthub.
	queriedEmail = ""
	if _, err := db.DB.Exec(`
		UPDATE local_card_selections SET selected_at=? WHERE local_id=?
	`, time.Now().Add(-publicCDKQueryWindow).Unix(), localID); err != nil {
		t.Fatal(err)
	}
	status, response = postBillingCheck(t, router, code)
	if status != http.StatusGone || response["status"] != "query_expired" {
		t.Fatalf("expired billing status=%d response=%v", status, response)
	}
	if queriedEmail != "" {
		t.Fatalf("expired billing queried accounthub with %q", queriedEmail)
	}
}

func TestActiveBindingBillingResolvesEmailFromCurrentResult(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("binds current result email and queries only that account", func(t *testing.T) {
		newLocalFixture(t)
		createLegacyLookupTables(t)
		const (
			code  = "SXC-ACTIVE-BILLING-RESULT-EMAIL"
			token = "active-billing-result-token"
			email = "active.billing@example.com"
		)
		if err := db.BindCDKRedemptionToken(code, token); err != nil {
			t.Fatal(err)
		}
		submittedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
		if err := db.ActivateCDKQueryWindowByToken(token, submittedAt); err != nil {
			t.Fatal(err)
		}

		var resultCalls, accountHubCalls atomic.Int32
		resultUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resultCalls.Add(1)
			if got := r.URL.Query().Get("token"); got != token {
				t.Errorf("result token=%q, want %q", got, token)
			}
			_ = json.NewEncoder(w).Encode(gin.H{
				"status": "success",
				"data": gin.H{"order": gin.H{
					"status":        "completed",
					"account_email": email,
				}},
			})
		}))
		defer resultUpstream.Close()
		t.Setenv("CARD_API_BASE", resultUpstream.URL)
		accountHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			accountHubCalls.Add(1)
			if got := r.URL.Query().Get("email"); got != email {
				t.Errorf("accounthub email=%q, want %q", got, email)
			}
			_ = json.NewEncoder(w).Encode(gin.H{"invoices": []gin.H{{
				"number":  "current-attempt-invoice",
				"created": submittedAt.Unix() + 30,
			}}})
		}))
		defer accountHub.Close()
		t.Setenv("ACCOUNTHUB_BASE_URL", accountHub.URL)
		router := gin.New()
		router.POST("/billing", SessionBillingCheck)

		status, response := postBillingCheck(t, router, code)
		if status != http.StatusOK {
			t.Fatalf("active result billing status=%d response=%v", status, response)
		}
		if resultCalls.Load() != 1 || accountHubCalls.Load() != 1 {
			t.Fatalf("active result billing calls=result:%d accounthub:%d", resultCalls.Load(), accountHubCalls.Load())
		}
		binding, err := db.GetBindingByCDK(code)
		if err != nil {
			t.Fatal(err)
		}
		if binding.AccountEmail != email {
			t.Fatalf("resolved current-attempt email was not bound: %+v", binding)
		}
		encoded, _ := json.Marshal(response)
		if strings.Contains(string(encoded), email) {
			t.Fatalf("active result billing leaked full email: %s", encoded)
		}
	})

	t.Run("result unavailable fails closed before accounthub", func(t *testing.T) {
		newLocalFixture(t)
		createLegacyLookupTables(t)
		const (
			code  = "SXC-ACTIVE-BILLING-RESULT-DOWN"
			token = "active-billing-result-down-token"
		)
		if err := db.BindCDKRedemptionToken(code, token); err != nil {
			t.Fatal(err)
		}
		if err := db.ActivateCDKQueryWindowByToken(token, time.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		var resultCalls, accountHubCalls atomic.Int32
		resultUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resultCalls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message":"private provider outage detail"}`))
		}))
		defer resultUpstream.Close()
		t.Setenv("CARD_API_BASE", resultUpstream.URL)
		accountHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			accountHubCalls.Add(1)
			_ = json.NewEncoder(w).Encode(gin.H{"invoices": []gin.H{}})
		}))
		defer accountHub.Close()
		t.Setenv("ACCOUNTHUB_BASE_URL", accountHub.URL)
		router := gin.New()
		router.POST("/billing", SessionBillingCheck)

		status, response := postBillingCheck(t, router, code)
		if status != http.StatusBadGateway {
			t.Fatalf("unavailable result billing status=%d response=%v", status, response)
		}
		if resultCalls.Load() != 1 || accountHubCalls.Load() != 0 {
			t.Fatalf("unavailable result calls=result:%d accounthub:%d", resultCalls.Load(), accountHubCalls.Load())
		}
		encoded, _ := json.Marshal(response)
		if strings.Contains(string(encoded), "private provider outage detail") {
			t.Fatalf("unavailable result billing leaked provider message: %s", encoded)
		}
	})
}

func TestActiveBindingBillingRejectsAttemptRotationDuringInvoiceLookup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newLocalFixture(t)
	createLegacyLookupTables(t)
	const (
		code       = "SXC-BILLING-ATTEMPT-ROTATION"
		token      = "billing-attempt-old-token"
		freshToken = "billing-attempt-fresh-token"
		email      = "old.billing.customer@example.com"
	)
	if err := db.BindCDKRedemptionToken(code, token); err != nil {
		t.Fatal(err)
	}
	submittedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := db.ActivateCDKQueryWindowByToken(token, submittedAt); err != nil {
		t.Fatal(err)
	}
	if err := db.BindCDKAccountEmail(code, token, email); err != nil {
		t.Fatal(err)
	}
	oldAttempt, err := db.GetBindingByCDK(code)
	if err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	accountHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(gin.H{"invoices": []gin.H{{
			"number":  "old-attempt-invoice",
			"created": submittedAt.Unix() + 30,
		}}})
	}))
	defer accountHub.Close()
	t.Setenv("ACCOUNTHUB_BASE_URL", accountHub.URL)
	router := gin.New()
	router.POST("/billing", SessionBillingCheck)

	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		body, _ := json.Marshal(gin.H{"cdk_code": code})
		req := httptest.NewRequest(http.MethodPost, "/billing", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		response <- recorder
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("billing request did not reach accounthub")
	}
	if _, err := db.BindCDKRedemptionTokenCASForFailedRetryWithAttempt(code, freshToken, oldAttempt); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	recorder := <-response
	if recorder.Code != http.StatusConflict {
		t.Fatalf("attempt-rotated billing status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "old-attempt-invoice") || strings.Contains(recorder.Body.String(), email) {
		t.Fatalf("attempt-rotated billing leaked stale invoice identity: %s", recorder.Body.String())
	}
}

func TestLocalBillingRejectsExactExpiryDuringInvoiceLookup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newLocalFixture(t)
	const (
		code  = "PULS-LOCAL-BILLING-EXPIRES-IN-FLIGHT"
		email = "expiring.billing.customer@example.com"
	)
	initialSubmittedAt := time.Now().UTC().Add(-time.Hour).Unix()
	localID := insertPublicLocalCDK(t, code, "consumed", email, initialSubmittedAt-60)
	if _, err := db.DB.Exec(`
		INSERT INTO local_card_selections(local_id, card_id, selected_at)
		VALUES (?, 456, ?)
	`, localID, initialSubmittedAt); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	accountHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(gin.H{"invoices": []gin.H{{
			"number":  "exact-boundary-invoice",
			"created": initialSubmittedAt + 30,
		}}})
	}))
	defer accountHub.Close()
	t.Setenv("ACCOUNTHUB_BASE_URL", accountHub.URL)
	router := gin.New()
	router.POST("/billing", SessionBillingCheck)

	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		body, _ := json.Marshal(gin.H{"cdk_code": code})
		req := httptest.NewRequest(http.MethodPost, "/billing", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		response <- recorder
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("billing request did not reach accounthub")
	}
	exactBoundary := time.Now().UTC().Add(-publicCDKQueryWindow).Unix()
	if _, err := db.DB.Exec(`
		UPDATE local_card_selections SET selected_at=? WHERE local_id=?
	`, exactBoundary, localID); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	recorder := <-response
	if recorder.Code != http.StatusGone {
		t.Fatalf("exact-expiry billing status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "exact-boundary-invoice") || strings.Contains(recorder.Body.String(), email) {
		t.Fatalf("exact-expiry billing leaked invoice identity: %s", recorder.Body.String())
	}
}

func TestLegacyBillingConflictingEmailsFailsClosed(t *testing.T) {
	newLocalFixture(t)
	createLegacyLookupTables(t)
	const (
		code       = "SXC-LEGACY-BILLING-FIRST-TASK"
		firstEmail = "first.billing@example.com"
		laterEmail = "later.private@example.com"
	)
	now := time.Now().UTC().Truncate(time.Second)
	firstCreated := now.Add(-time.Hour)
	laterCreated := now.Add(-5 * time.Minute)
	if _, err := db.DB.Exec(`
		INSERT INTO cd_keys(code, plan_type, status, used_at)
		VALUES (?, 'plus', 'used', ?);
		INSERT INTO recharge_tasks
			(cdk_code, task_status, account_email, notes, created_at)
		VALUES (?, 'processing', ?, 'first internal note', ?);
		INSERT INTO recharge_tasks
			(cdk_code, task_status, account_email, notes, created_at)
		VALUES (?, 'completed', ?, 'later internal note', ?)
	`, code, firstCreated.Add(20*time.Minute).Format("2006-01-02 15:04:05"),
		code, firstEmail, firstCreated.Format("2006-01-02 15:04:05"),
		code, laterEmail, laterCreated.Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	accountHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(gin.H{"invoices": []gin.H{}})
	}))
	defer accountHub.Close()
	t.Setenv("ACCOUNTHUB_BASE_URL", accountHub.URL)
	router := gin.New()
	router.POST("/billing", SessionBillingCheck)

	status, response := postBillingCheck(t, router, code)
	if status != http.StatusNotFound {
		t.Fatalf("conflicting legacy identities status=%d response=%v", status, response)
	}
	if calls.Load() != 0 {
		t.Fatalf("conflicting legacy identities called accounthub %d times", calls.Load())
	}
	encoded, _ := json.Marshal(response)
	if strings.Contains(string(encoded), firstEmail) || strings.Contains(string(encoded), laterEmail) ||
		strings.Contains(string(encoded), "internal note") {
		t.Fatalf("legacy billing response leaked identity or notes: %s", encoded)
	}
}

func TestLegacyBillingLaterTaskCannotRenewExpiredWindow(t *testing.T) {
	newLocalFixture(t)
	createLegacyLookupTables(t)
	const code = "SXC-LEGACY-BILLING-NO-RENEWAL"
	now := time.Now().UTC().Truncate(time.Second)
	oldCreated := now.Add(-publicCDKQueryWindow - time.Minute)
	recentCreated := now.Add(-time.Minute)
	if _, err := db.DB.Exec(`
		INSERT INTO cd_keys(code, plan_type, status, used_at)
		VALUES (?, 'plus', 'used', ?);
		INSERT INTO recharge_tasks
			(cdk_code, task_status, account_email, notes, created_at)
		VALUES (?, 'failed', 'same.private@example.com', 'old internal note', ?);
		INSERT INTO recharge_tasks
			(cdk_code, task_status, account_email, notes, created_at)
		VALUES (?, 'completed', 'same.private@example.com', 'new internal note', ?)
	`, code, recentCreated.Format("2006-01-02 15:04:05"),
		code, oldCreated.Format("2006-01-02 15:04:05"),
		code, recentCreated.Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	accountHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(gin.H{"invoices": []gin.H{}})
	}))
	defer accountHub.Close()
	t.Setenv("ACCOUNTHUB_BASE_URL", accountHub.URL)
	router := gin.New()
	router.POST("/billing", SessionBillingCheck)

	status, response := postBillingCheck(t, router, code)
	if status != http.StatusGone || response["status"] != "query_expired" {
		t.Fatalf("later task renewed legacy billing window: status=%d response=%v", status, response)
	}
	if calls.Load() != 0 {
		t.Fatalf("expired legacy billing called accounthub %d times", calls.Load())
	}
	encoded, _ := json.Marshal(response)
	for _, forbidden := range []string{"same.private@example.com", "internal note"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("expired legacy billing leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestLegacyBillingUsesUniqueEmailWhenEarliestTaskEmailIsEmpty(t *testing.T) {
	newLocalFixture(t)
	createLegacyLookupTables(t)
	const code = "SXC-LEGACY-BILLING-NO-LATER-EMAIL"
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := db.DB.Exec(`
		INSERT INTO cd_keys(code, plan_type, status, used_at)
		VALUES (?, 'plus', 'used', ?);
		INSERT INTO recharge_tasks
			(cdk_code, task_status, account_email, notes, created_at)
		VALUES (?, 'processing', '', 'first internal note', ?);
		INSERT INTO recharge_tasks
			(cdk_code, task_status, account_email, notes, created_at)
		VALUES (?, 'completed', 'later.private@example.com', 'later internal note', ?)
	`, code, now.Add(-30*time.Minute).Format("2006-01-02 15:04:05"),
		code, now.Add(-time.Hour).Format("2006-01-02 15:04:05"),
		code, now.Add(-5*time.Minute).Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	var queriedEmail atomic.Value
	accountHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		queriedEmail.Store(r.URL.Query().Get("email"))
		_ = json.NewEncoder(w).Encode(gin.H{"invoices": []gin.H{}})
	}))
	defer accountHub.Close()
	t.Setenv("ACCOUNTHUB_BASE_URL", accountHub.URL)
	router := gin.New()
	router.POST("/billing", SessionBillingCheck)

	status, response := postBillingCheck(t, router, code)
	if status != http.StatusOK {
		t.Fatalf("unique later email status=%d response=%v", status, response)
	}
	if calls.Load() != 1 {
		t.Fatalf("unique later email accounthub calls=%d", calls.Load())
	}
	if got, _ := queriedEmail.Load().(string); got != "later.private@example.com" {
		t.Fatalf("unique later email query=%q", got)
	}
	encoded, _ := json.Marshal(response)
	if strings.Contains(string(encoded), "later.private@example.com") || strings.Contains(string(encoded), "internal note") {
		t.Fatalf("unique later email billing leaked raw identity: %s", encoded)
	}
}

func TestLegacyBillingFailsClosedWhenAnyTaskLacksImmutableTimestamp(t *testing.T) {
	newLocalFixture(t)
	createLegacyLookupTables(t)
	const (
		code  = "SXC-LEGACY-BILLING-UNANCHORED-TASK"
		email = "anchored.private@example.com"
	)
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := db.DB.Exec(`
		INSERT INTO cd_keys(code, plan_type, status, used_at)
		VALUES (?, 'plus', 'used', ?);
		INSERT INTO recharge_tasks(cdk_code, task_status, account_email, notes, created_at)
		VALUES (?, 'completed', ?, 'anchored internal note', ?);
		INSERT INTO recharge_tasks(cdk_code, task_status, account_email, notes, created_at)
		VALUES (?, 'processing', ?, 'unanchored internal note', NULL)
	`, code, now.Add(-30*time.Minute).Format("2006-01-02 15:04:05"),
		code, email, now.Add(-time.Hour).Format("2006-01-02 15:04:05"), code, email); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	accountHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(gin.H{"invoices": []gin.H{}})
	}))
	defer accountHub.Close()
	t.Setenv("ACCOUNTHUB_BASE_URL", accountHub.URL)
	router := gin.New()
	router.POST("/billing", SessionBillingCheck)

	status, response := postBillingCheck(t, router, code)
	if status != http.StatusNotFound {
		t.Fatalf("unanchored legacy billing status=%d response=%v", status, response)
	}
	if calls.Load() != 0 {
		t.Fatalf("unanchored legacy billing called accounthub %d times", calls.Load())
	}
	encoded, _ := json.Marshal(response)
	for _, forbidden := range []string{email, "anchored internal note", "unanchored internal note"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("unanchored legacy billing leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestLegacyBillingWindowUsesEarliestTaskOrUsedAt(t *testing.T) {
	newLocalFixture(t)
	createLegacyLookupTables(t)
	fixedNow := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	t.Run("exact seven-day task boundary is expired", func(t *testing.T) {
		const code = "SXC-LEGACY-BILLING-EXACT-BOUNDARY"
		firstCreated := fixedNow.Add(-publicCDKQueryWindow)
		if _, err := db.DB.Exec(`
			INSERT INTO cd_keys(code, plan_type, status, used_at)
			VALUES (?, 'plus', 'used', ?);
			INSERT INTO recharge_tasks
				(cdk_code, task_status, account_email, created_at)
			VALUES (?, 'completed', 'exact@example.com', ?)
		`, code, fixedNow.Add(-time.Hour).Format("2006-01-02 15:04:05"),
			code, firstCreated.Format("2006-01-02 15:04:05")); err != nil {
			t.Fatal(err)
		}

		email, submittedAt, expiresAt, found, err := legacyCDKBillingIdentity(code, fixedNow)
		if err != db.ErrCDKQueryExpired || !found || email != "" {
			t.Fatalf("exact boundary result email=%q submitted=%d expires=%d found=%v err=%v",
				email, submittedAt, expiresAt, found, err)
		}
		if submittedAt != firstCreated.Unix() || expiresAt != fixedNow.Unix() {
			t.Fatalf("exact boundary submitted=%d expires=%d", submittedAt, expiresAt)
		}
	})

	t.Run("earlier used-at shortens task window", func(t *testing.T) {
		const code = "SXC-LEGACY-BILLING-EARLIER-USED-AT"
		firstCreated := fixedNow.Add(-2 * time.Hour)
		usedAt := firstCreated.Add(-time.Hour)
		if _, err := db.DB.Exec(`
			INSERT INTO cd_keys(code, plan_type, status, used_at)
			VALUES (?, 'plus', 'used', ?);
			INSERT INTO recharge_tasks
				(cdk_code, task_status, account_email, created_at)
			VALUES (?, 'completed', 'anchor@example.com', ?)
		`, code, usedAt.Format("2006-01-02 15:04:05"),
			code, firstCreated.Format("2006-01-02 15:04:05")); err != nil {
			t.Fatal(err)
		}

		email, submittedAt, expiresAt, found, err := legacyCDKBillingIdentity(code, fixedNow)
		if err != nil || !found || email != "anchor@example.com" {
			t.Fatalf("earlier used-at result email=%q found=%v err=%v", email, found, err)
		}
		if submittedAt != usedAt.Unix() || expiresAt != usedAt.Add(publicCDKQueryWindow).Unix() {
			t.Fatalf("window anchor submitted=%d expires=%d, want %d/%d",
				submittedAt, expiresAt, usedAt.Unix(), usedAt.Add(publicCDKQueryWindow).Unix())
		}
	})
}

func TestSafePublicResultPayloadRedactsSecrets(t *testing.T) {
	rawEmail := "very.private.customer@example.com"
	payload := map[string]any{
		"status":           "completed",
		"message":          "top-message-secret redemption-secret 4242424242424242",
		"redemption_token": "redemption-secret",
		"cdk_code":         "PULS-RAW-CODE",
		"internal_notes":   "operator-only-note",
		"notes":            "provider-only-note",
		"card_id":          7788,
		"card_number":      "4242424242424242",
		"account_email":    rawEmail,
		"order": map[string]any{
			"status":           "completed",
			"message":          "order-message-secret nested-token",
			"account_email":    rawEmail,
			"redemption_token": "nested-token",
			"card_id":          7788,
			"provider_result":  "private-provider-result",
			"internal_notes":   "nested-operator-note",
		},
		"events": []any{
			map[string]any{
				"stage":             "done",
				"message":           "event-message-secret event-provider-secret",
				"internal_notes":    "event-operator-note",
				"provider_response": "event-provider-secret",
			},
		},
	}

	safe := safePublicResultPayload(payload)
	encoded, err := json.Marshal(safe)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{
		"redemption-secret", "nested-token", "PULS-RAW-CODE",
		"top-message-secret", "order-message-secret", "event-message-secret",
		"operator-only-note", "provider-only-note", "nested-operator-note",
		"event-operator-note", "private-provider-result", "event-provider-secret",
		"4242424242424242", rawEmail,
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("safe result leaked %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "@example.com") || !strings.Contains(text, "*") {
		t.Fatalf("safe result did not retain a masked email: %s", text)
	}
	for _, forbiddenKey := range []string{
		"redemption_token", "cdk_code", "internal_notes", "notes",
		"card_id", "card_number", "provider_result", "provider_response",
	} {
		if strings.Contains(text, `"`+forbiddenKey+`"`) {
			t.Fatalf("safe result retained forbidden key %q: %s", forbiddenKey, text)
		}
	}
}

func TestSafePublicResultPayloadPrefersNestedOrderStatusOverSuccessEnvelope(t *testing.T) {
	for _, test := range []struct {
		innerStatus string
		wantStatus  string
	}{
		{innerStatus: "failed", wantStatus: "failed"},
		{innerStatus: "processing", wantStatus: "processing"},
	} {
		t.Run(test.innerStatus, func(t *testing.T) {
			safe := safePublicResultPayload(map[string]any{
				"status": "success",
				"data": map[string]any{
					"order": map[string]any{
						"status":  test.innerStatus,
						"message": "UPSTREAM private nested message",
					},
				},
			})
			if safe["status"] != test.wantStatus {
				t.Fatalf("safe status=%v, want nested order status %q; payload=%v",
					safe["status"], test.wantStatus, safe)
			}
			if safe["status"] == "completed" {
				t.Fatalf("success envelope overrode nested order: %v", safe)
			}
			order, ok := safe["order"].(map[string]any)
			if !ok || order["status"] != test.wantStatus {
				t.Fatalf("safe order=%T %v, want status %q", safe["order"], safe["order"], test.wantStatus)
			}
			encoded, _ := json.Marshal(safe)
			if strings.Contains(string(encoded), "UPSTREAM private nested message") {
				t.Fatalf("safe payload leaked nested provider message: %s", encoded)
			}
		})
	}
}

func TestSafePublicResultEventsMatchRechargeViewContract(t *testing.T) {
	payload := map[string]any{
		"status": "processing",
		"events": []any{
			map[string]any{
				"step":           "payment",
				"category":       "success",
				"to_status":      "paid",
				"public_message": "UPSTREAM payment token=payment-secret card=4242424242424242",
				"message":        "UPSTREAM message payment-secret",
				"created_at":     "2026-09-19T10:00:00Z",
			},
			map[string]any{
				"step":           "reconcile",
				"category":       "provider-private-category",
				"to_status":      "failed_precharge",
				"public_message": "UPSTREAM failure secret=failure-secret",
				"created_at":     "2026-09-19T10:01:00Z",
			},
			map[string]any{
				"step":           "<script>unknown-internal-step",
				"category":       "success",
				"to_status":      "completed",
				"public_message": "unknown-step-secret",
				"created_at":     "2026-09-19T10:02:00Z",
			},
		},
	}

	safe := safePublicResultPayload(payload)
	events, ok := safe["events"].([]map[string]any)
	if !ok || len(events) != 2 {
		t.Fatalf("safe events=%T %v, want two RechargeView events", safe["events"], safe["events"])
	}
	steps := map[string]bool{
		"queued": true, "credential_check": true, "pricing": true,
		"checkout": true, "payment": true, "subscription": true,
		"invoice": true, "renewal": true, "reconcile": true, "completed": true,
	}
	categories := map[string]bool{"info": true, "warning": true, "success": true, "error": true}
	statuses := map[string]bool{
		"queued": true, "pending": true, "processing": true, "running": true,
		"submitted": true, "review": true, "completed": true, "failed": true,
	}
	allowedKeys := map[string]bool{
		"step": true, "category": true, "to_status": true,
		"public_message": true, "created_at": true,
	}
	for i, event := range events {
		for key := range event {
			if !allowedKeys[key] {
				t.Fatalf("event %d exposed unsupported key %q: %v", i, key, event)
			}
		}
		step, _ := event["step"].(string)
		category, _ := event["category"].(string)
		status, _ := event["to_status"].(string)
		message, _ := event["public_message"].(string)
		if !steps[step] || !categories[category] || !statuses[status] || message == "" {
			t.Fatalf("event %d violates RechargeView contract: %v", i, event)
		}
	}
	if events[0]["step"] != "payment" || events[0]["category"] != "success" ||
		events[0]["to_status"] != "completed" || events[0]["public_message"] != "该步骤已完成。" {
		t.Fatalf("completed payment event was not safely normalized: %v", events[0])
	}
	if events[1]["step"] != "reconcile" || events[1]["category"] != "error" ||
		events[1]["to_status"] != "failed" || events[1]["public_message"] != "该步骤未完成；如需协助，请联系客服。" {
		t.Fatalf("failed reconcile event was not safely normalized: %v", events[1])
	}
	encoded, _ := json.Marshal(safe)
	text := string(encoded)
	for _, secret := range []string{
		"UPSTREAM", "payment-secret", "failure-secret", "unknown-step-secret",
		"4242424242424242", "provider-private-category", "unknown-internal-step",
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("safe events leaked %q: %s", secret, text)
		}
	}
}

func TestSafeCDKInvoiceOnlyReturnsNearbySanitizedInvoice(t *testing.T) {
	const submittedAt int64 = 2_000_000_000
	invoices := []map[string]any{
		{
			"number":             "outside-window",
			"created":            submittedAt + int64(3*time.Hour/time.Second),
			"hosted_invoice_url": "https://secret.example/outside",
		},
		{
			"number":      "near-but-not-closest",
			"status":      "paid",
			"created":     submittedAt - 10*60,
			"invoice_url": "https://secret.example/near-old",
		},
		{
			"number":             "this-redemption",
			"status":             "paid",
			"created":            submittedAt + 45,
			"amount_paid":        2000,
			"hosted_invoice_url": "https://secret.example/near",
			"invoice_pdf":        "https://secret.example/pdf",
			"customer_email":     "private@example.com",
			"metadata":           map[string]any{"secret": "hidden"},
		},
	}

	got := safeCDKInvoice(invoices, submittedAt)
	if len(got) != 1 || got[0]["number"] != "this-redemption" {
		t.Fatalf("safeCDKInvoice=%v", got)
	}
	encoded, _ := json.Marshal(got)
	text := string(encoded)
	for _, forbidden := range []string{"secret.example", "private@example.com", "metadata", "outside-window"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("safe invoice leaked %q: %s", forbidden, text)
		}
	}

	farOnly := safeCDKInvoice([]map[string]any{{
		"number":  "another-order",
		"created": submittedAt + int64(24*time.Hour/time.Second),
	}}, submittedAt)
	if len(farOnly) != 0 {
		t.Fatalf("unrelated later invoice was returned: %v", farOnly)
	}
}
