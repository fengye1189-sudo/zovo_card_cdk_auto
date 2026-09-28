package db

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

var (
	ErrManagedAttemptActive  = errors.New("managed activation attempt already active")
	ErrManagedAttemptChanged = errors.New("managed activation attempt changed")
	ErrManagedSubmitClaimed  = errors.New("managed activation submit already claimed")
)

const managedQueryWindow = 7 * 24 * time.Hour

type ManagedActivationAttempt struct {
	CodeHash              string
	AttemptHash           string
	PreflightHash         string
	SessionHash           string
	TaskID                string
	Plan                  string
	Status                string
	AccountEmail          string
	FailureReason         string
	SubmitClaimedAt       int64
	QueryStartedAt        int64
	QueryExpiresAt        int64
	CreatedAt             int64
	UpdatedAt             int64
	ActivatedAt           int64
	SubscriptionExpiresAt int64
	ExpiryEstimated       bool
}

func InitManagedActivation() error {
	if DB == nil {
		return sql.ErrConnDone
	}
	_, err := DB.Exec(`
		CREATE TABLE IF NOT EXISTS managed_activation_attempts (
			code_hash TEXT PRIMARY KEY,
			attempt_hash TEXT NOT NULL UNIQUE,
			preflight_hash TEXT NOT NULL DEFAULT '',
			session_hash TEXT NOT NULL DEFAULT '',
			task_id TEXT NOT NULL DEFAULT '',
			plan TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'verified',
			account_email TEXT NOT NULL DEFAULT '',
			failure_reason TEXT NOT NULL DEFAULT '',
			submit_claimed_at INTEGER NOT NULL DEFAULT 0,
			query_started_at INTEGER NOT NULL DEFAULT 0,
			query_expires_at INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_managed_activation_attempt
		ON managed_activation_attempts(attempt_hash);
		CREATE INDEX IF NOT EXISTS idx_managed_activation_expiry
		ON managed_activation_attempts(query_expires_at);
	`)
	if err != nil {
		return err
	}
	for _, migration := range []string{
		`ALTER TABLE managed_activation_attempts ADD COLUMN activated_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE managed_activation_attempts ADD COLUMN subscription_expires_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE managed_activation_attempts ADD COLUMN expiry_estimated INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err = DB.Exec(migration); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return err
		}
	}
	_, err = DB.Exec(`UPDATE managed_activation_attempts
		SET activated_at=CASE WHEN activated_at<=0 THEN updated_at ELSE activated_at END,
		    subscription_expires_at=CASE WHEN subscription_expires_at<=0
		      THEN CAST(strftime('%s',datetime(updated_at,'unixepoch','+1 month')) AS INTEGER)
		      ELSE subscription_expires_at END,
		    expiry_estimated=1
		WHERE LOWER(TRIM(status))='completed' AND TRIM(account_email)<>''
		  AND (activated_at<=0 OR subscription_expires_at<=0)`)
	return err
}

func BeginManagedActivation(codeHash, attemptHash, plan string, now time.Time) error {
	if DB == nil {
		return sql.ErrConnDone
	}
	nowUnix := now.UTC().Unix()
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var existing ManagedActivationAttempt
	err = scanManagedAttempt(tx.QueryRow(`
		SELECT code_hash,attempt_hash,preflight_hash,session_hash,task_id,plan,status,
		       account_email,failure_reason,submit_claimed_at,query_started_at,query_expires_at,
		       created_at,updated_at,activated_at,subscription_expires_at,expiry_estimated
		FROM managed_activation_attempts WHERE code_hash=?
	`, codeHash), &existing)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil && existing.SubmitClaimedAt > 0 && existing.Status != "failed" && existing.Status != "not_submitted" {
		return ErrManagedAttemptActive
	}
	_, err = tx.Exec(`
		INSERT INTO managed_activation_attempts(
			code_hash,attempt_hash,plan,status,created_at,updated_at
		) VALUES(?,?,?,'verified',?,?)
		ON CONFLICT(code_hash) DO UPDATE SET
			attempt_hash=excluded.attempt_hash,preflight_hash='',session_hash='',task_id='',
			plan=excluded.plan,status='verified',account_email='',failure_reason='',
			submit_claimed_at=0,query_started_at=0,query_expires_at=0,updated_at=excluded.updated_at
	`, codeHash, attemptHash, plan, nowUnix, nowUnix)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func SaveManagedPreflight(codeHash, attemptHash, preflightHash, sessionHash, email string, now time.Time) error {
	res, err := DB.Exec(`
		UPDATE managed_activation_attempts
		SET preflight_hash=?,session_hash=?,account_email=?,status='preflight',updated_at=?
		WHERE code_hash=? AND attempt_hash=? AND submit_claimed_at=0
	`, preflightHash, sessionHash, email, now.UTC().Unix(), codeHash, attemptHash)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrManagedAttemptChanged
	}
	return nil
}

func ClaimManagedActivation(codeHash, attemptHash, preflightHash, sessionHash string, now time.Time) error {
	nowUnix := now.UTC().Unix()
	res, err := DB.Exec(`
		UPDATE managed_activation_attempts
		SET submit_claimed_at=?,query_started_at=?,query_expires_at=?,status='submitted',updated_at=?
		WHERE code_hash=? AND attempt_hash=? AND preflight_hash=? AND session_hash=?
		  AND submit_claimed_at=0
	`, nowUnix, nowUnix, now.Add(managedQueryWindow).UTC().Unix(), nowUnix,
		codeHash, attemptHash, preflightHash, sessionHash)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	row, getErr := GetManagedActivationByCodeHash(codeHash)
	if getErr == nil && row != nil && row.SubmitClaimedAt > 0 {
		return ErrManagedSubmitClaimed
	}
	return ErrManagedAttemptChanged
}

func ReleaseManagedActivationClaim(codeHash, attemptHash string, now time.Time) error {
	res, err := DB.Exec(`
		UPDATE managed_activation_attempts
		SET submit_claimed_at=0,query_started_at=0,query_expires_at=0,
		    preflight_hash='',session_hash='',status='not_submitted',updated_at=?
		WHERE code_hash=? AND attempt_hash=? AND task_id=''
	`, now.UTC().Unix(), codeHash, attemptHash)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrManagedAttemptChanged
	}
	return nil
}

func RecordManagedActivationSubmission(codeHash, attemptHash, taskID, status string, now time.Time) error {
	res, err := DB.Exec(`
		UPDATE managed_activation_attempts
		SET task_id=?,status=?,updated_at=?
		WHERE code_hash=? AND attempt_hash=? AND submit_claimed_at>0
	`, taskID, status, now.UTC().Unix(), codeHash, attemptHash)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrManagedAttemptChanged
	}
	return nil
}

func RecordManagedActivationResult(codeHash, taskID, plan, status, email, failureReason string, now time.Time) error {
	return RecordManagedActivationResultDetails(codeHash, taskID, plan, status, email, failureReason, 0, 0, now)
}

// RecordManagedActivationResultDetails persists the first confirmed completion
// window. Repeated public lookups are idempotent and cannot move a customer's
// renewal date forward.
func RecordManagedActivationResultDetails(codeHash, taskID, plan, status, email, failureReason string, activatedAt, expiresAt int64, now time.Time) error {
	_, err := DB.Exec(`
		UPDATE managed_activation_attempts
		SET task_id=CASE WHEN ?<>'' THEN ? ELSE task_id END,
		    plan=CASE WHEN ?<>'' THEN ? ELSE plan END,status=?,
		    account_email=CASE WHEN ?<>'' THEN ? ELSE account_email END,
		    failure_reason=?,updated_at=?,
		    activated_at=CASE WHEN LOWER(TRIM(?))='completed' AND activated_at<=0 AND ?>0 THEN ? ELSE activated_at END,
		    subscription_expires_at=CASE WHEN LOWER(TRIM(?))='completed' AND subscription_expires_at<=0 AND ?>? THEN ? ELSE subscription_expires_at END,
		    expiry_estimated=CASE WHEN LOWER(TRIM(?))='completed' AND ?>0 AND ?>? THEN 1 ELSE expiry_estimated END
		WHERE code_hash=?
	`, taskID, taskID, plan, plan, status, email, email, failureReason, now.UTC().Unix(),
		status, activatedAt, activatedAt, status, expiresAt, activatedAt, expiresAt,
		status, activatedAt, expiresAt, activatedAt, codeHash)
	return err
}

func GetManagedActivationByCodeHash(codeHash string) (*ManagedActivationAttempt, error) {
	if DB == nil {
		return nil, sql.ErrConnDone
	}
	var row ManagedActivationAttempt
	err := scanManagedAttempt(DB.QueryRow(`
		SELECT code_hash,attempt_hash,preflight_hash,session_hash,task_id,plan,status,
		       account_email,failure_reason,submit_claimed_at,query_started_at,query_expires_at,
		       created_at,updated_at,activated_at,subscription_expires_at,expiry_estimated
		FROM managed_activation_attempts WHERE code_hash=?
	`, codeHash), &row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func GetManagedActivationByAttemptHash(attemptHash string) (*ManagedActivationAttempt, error) {
	if DB == nil {
		return nil, sql.ErrConnDone
	}
	var row ManagedActivationAttempt
	err := scanManagedAttempt(DB.QueryRow(`
		SELECT code_hash,attempt_hash,preflight_hash,session_hash,task_id,plan,status,
		       account_email,failure_reason,submit_claimed_at,query_started_at,query_expires_at,
		       created_at,updated_at,activated_at,subscription_expires_at,expiry_estimated
		FROM managed_activation_attempts WHERE attempt_hash=?
	`, attemptHash), &row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanManagedAttempt(row rowScanner, out *ManagedActivationAttempt) error {
	var estimated int
	err := row.Scan(&out.CodeHash, &out.AttemptHash, &out.PreflightHash, &out.SessionHash,
		&out.TaskID, &out.Plan, &out.Status, &out.AccountEmail, &out.FailureReason,
		&out.SubmitClaimedAt, &out.QueryStartedAt, &out.QueryExpiresAt,
		&out.CreatedAt, &out.UpdatedAt, &out.ActivatedAt, &out.SubscriptionExpiresAt, &estimated)
	out.ExpiryEstimated = estimated == 1
	return err
}
