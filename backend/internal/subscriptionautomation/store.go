package subscriptionautomation

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/db"
)

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

func boolInt(v bool) int { if v { return 1 }; return 0 }
