package subscriptionautomation

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/db"
)

type WatchTask struct {
	OrderID string
	Provider string
	ExternalID string
	Attempts int
	Deadline time.Time
}

// Store persists only subscription-automation state. It deliberately has no
// accessors for CDK, card inventory, or legacy recharge tables.
type Store struct{ database *sql.DB }

func NewStore(database *sql.DB) *Store {
	if database == nil { database = db.DB }
	return &Store{database: database}
}

func (s *Store) CreateOrder(orderID, clientOrderNo, product string) error {
	if s == nil || s.database == nil { return fmt.Errorf("subscription database is not configured") }
	if orderID == "" || clientOrderNo == "" || product == "" { return fmt.Errorf("subscription order fields are required") }
	_, err := s.database.Exec(`INSERT INTO subscription_orders(order_id,client_order_no,product) VALUES(?,?,?)`, orderID, clientOrderNo, product)
	return err
}

func (s *Store) PrimaryCounts(window int) (map[string]int, error) {
	if s == nil || s.database == nil { return nil, fmt.Errorf("subscription database is not configured") }
	if window < 1 || window > 10000 { window = 1000 }
	rows, err := s.database.Query(`SELECT provider,COUNT(*) FROM (SELECT provider FROM subscription_order_attempts WHERE provider_role='PRIMARY' ORDER BY id DESC LIMIT ?) GROUP BY provider`, window)
	if err != nil { return nil, err }
	defer rows.Close()
	out := map[string]int{"zovo": 0, "orbitcard": 0}
	for rows.Next() { var name string; var count int; if err := rows.Scan(&name, &count); err != nil { return nil, err }; out[name] = count }
	return out, rows.Err()
}

func (s *Store) ProviderHealth() (map[string]HealthState, error) {
	if s == nil || s.database == nil { return nil, fmt.Errorf("subscription database is not configured") }
	out := map[string]HealthState{"zovo": HealthDegraded, "orbitcard": HealthDegraded}
	rows, err := s.database.Query(`SELECT provider,state FROM subscription_provider_health WHERE provider IN ('zovo','orbitcard')`)
	if err != nil { return nil, err }
	defer rows.Close()
	for rows.Next() { var name, state string; if err := rows.Scan(&name, &state); err != nil { return nil, err }; out[name] = HealthState(state) }
	return out, rows.Err()
}

func (s *Store) UpsertProviderHealth(providerName string, state HealthState, message string) error {
	if s == nil || s.database == nil { return fmt.Errorf("subscription database is not configured") }
	_, err := s.database.Exec(`INSERT INTO subscription_provider_health(provider,state,message,checked_at) VALUES(?,?,?,CURRENT_TIMESTAMP) ON CONFLICT(provider) DO UPDATE SET state=excluded.state,message=excluded.message,checked_at=CURRENT_TIMESTAMP`, providerName, state, message)
	return err
}

func (s *Store) CreatePreviewOrder(orderID, clientOrderNo, product, selectedProvider string) error {
	if s == nil || s.database == nil { return fmt.Errorf("subscription database is not configured") }
	if orderID == "" || clientOrderNo == "" || product == "" || selectedProvider == "" { return fmt.Errorf("subscription preview fields are required") }
	tx, err := s.database.Begin(); if err != nil { return err }
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`INSERT INTO subscription_orders(order_id,client_order_no,product,status,initial_provider) VALUES(?,?,?,'PREVIEW_ONLY',?)`, orderID, clientOrderNo, product, selectedProvider)
	if err != nil { return err }
	_, err = tx.Exec(`INSERT INTO subscription_order_attempts(order_id,attempt_no,provider,provider_role,client_order_no,idempotency_key,status) VALUES(?,1,?,'PRIMARY',?,?,'PREVIEW_ONLY')`, orderID, selectedProvider, clientOrderNo, clientOrderNo)
	if err != nil { return err }
	return tx.Commit()
}

func (s *Store) RecordAttempt(orderID string, attemptNo int, provider, role, clientOrderNo, idem, externalID, status, rawStatus, reason string, unknown, terminal bool) error {
	if s == nil || s.database == nil { return fmt.Errorf("subscription database is not configured") }
	if attemptNo < 1 || orderID == "" || provider == "" || clientOrderNo == "" || idem == "" { return fmt.Errorf("subscription attempt fields are required") }
	_, err := s.database.Exec(`INSERT INTO subscription_order_attempts(order_id,attempt_no,provider,provider_role,client_order_no,idempotency_key,external_order_id,status,raw_status,is_unknown,is_terminal_failure,failure_reason,finished_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,CASE WHEN ? IN ('SUCCEEDED','FAILED') THEN CURRENT_TIMESTAMP ELSE NULL END)`, orderID, attemptNo, provider, role, clientOrderNo, idem, externalID, status, rawStatus, boolInt(unknown), boolInt(terminal), reason, status)
	return err
}

func (s *Store) ScheduleWatch(orderID, provider, externalID string, deadline time.Time) error {
	if s == nil || s.database == nil { return fmt.Errorf("subscription database is not configured") }
	if orderID == "" || provider == "" || externalID == "" { return fmt.Errorf("watcher fields are required") }
	_, err := s.database.Exec(`INSERT INTO subscription_order_watchers(order_id,provider,external_order_id,next_check_at,deadline_at) VALUES(?,?,?,?,?) ON CONFLICT(order_id) DO UPDATE SET provider=excluded.provider,external_order_id=excluded.external_order_id,status='ACTIVE',next_check_at=excluded.next_check_at,deadline_at=excluded.deadline_at,updated_at=CURRENT_TIMESTAMP`, orderID, provider, externalID, time.Now().UTC().Add(2*time.Second), deadline.UTC())
	return err
}

func (s *Store) ClaimDueWatcher() (*WatchTask, error) {
	if s == nil || s.database == nil { return nil, fmt.Errorf("subscription database is not configured") }
	tx, err := s.database.Begin(); if err != nil { return nil, err }
	defer func() { _ = tx.Rollback() }()
	var task WatchTask
	var deadline string
	err = tx.QueryRow(`SELECT order_id,provider,external_order_id,attempts,deadline_at FROM subscription_order_watchers WHERE status='ACTIVE' AND next_check_at<=CURRENT_TIMESTAMP AND (lease_until IS NULL OR lease_until<CURRENT_TIMESTAMP) ORDER BY next_check_at LIMIT 1`).Scan(&task.OrderID, &task.Provider, &task.ExternalID, &task.Attempts, &deadline)
	if err == sql.ErrNoRows { return nil, nil }
	if err != nil { return nil, err }
	if parsed, parseErr := time.Parse("2006-01-02 15:04:05", deadline); parseErr == nil { task.Deadline = parsed.UTC() } else { task.Deadline = time.Now().UTC().Add(15*time.Minute) }
	_, err = tx.Exec(`UPDATE subscription_order_watchers SET lease_until=datetime('now','+20 seconds'),attempts=attempts+1,updated_at=CURRENT_TIMESTAMP WHERE order_id=? AND status='ACTIVE'`, task.OrderID)
	if err != nil { return nil, err }
	if err = tx.Commit(); err != nil { return nil, err }
	task.Attempts++
	return &task, nil
}

func (s *Store) RescheduleWatcher(orderID, reason string, delay time.Duration, deadlineReached bool) error {
	if s == nil || s.database == nil { return fmt.Errorf("subscription database is not configured") }
	if deadlineReached { _, err := s.database.Exec(`UPDATE subscription_order_watchers SET status='MANUAL_REVIEW',lease_until=NULL,last_error=?,updated_at=CURRENT_TIMESTAMP WHERE order_id=?`, reason, orderID); return err }
	seconds := int(delay.Seconds()); if seconds < 1 { seconds = 1 }
	_, err := s.database.Exec(`UPDATE subscription_order_watchers SET next_check_at=datetime('now', '+' || ? || ' seconds'),lease_until=NULL,last_error=?,updated_at=CURRENT_TIMESTAMP WHERE order_id=?`, seconds, reason, orderID)
	return err
}

func (s *Store) FinishWatcher(orderID, rawStatus, externalID, finalStatus, reason string) error {
	if s == nil || s.database == nil { return fmt.Errorf("subscription database is not configured") }
	_, err := s.database.Exec(`UPDATE subscription_order_watchers SET status=?,external_order_id=COALESCE(NULLIF(?,''),external_order_id),lease_until=NULL,last_error=?,updated_at=CURRENT_TIMESTAMP WHERE order_id=?`, finalStatus, externalID, reason, orderID)
	if err != nil { return err }
	if finalStatus == "SUCCEEDED" { _, err = s.database.Exec(`UPDATE subscription_orders SET status='SUCCEEDED',final_provider=(SELECT provider FROM subscription_order_watchers WHERE order_id=?),updated_at=CURRENT_TIMESTAMP WHERE order_id=?`, orderID, orderID) }
	if finalStatus == "TERMINAL_FAILURE" { _, err = s.database.Exec(`UPDATE subscription_orders SET last_error=?,updated_at=CURRENT_TIMESTAMP WHERE order_id=?`, reason, orderID) }
	return err
}

func boolInt(v bool) int { if v { return 1 }; return 0 }
