package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/db"
)

const customerExpiryReminderInterval = 5 * time.Minute

var customerExpiryReminderMu sync.Mutex

type customerExpiryReminder struct {
	EventID        string
	LocalID        int64
	AccountEmail   string
	Plan           string
	ActivatedAt    int64
	DueAt          int64
	ExpiryEstimated bool
}

func InitCustomerExpiryNotifications() error {
	_, err := db.DB.Exec(`CREATE TABLE IF NOT EXISTS customer_expiry_notifications (
		event_id TEXT PRIMARY KEY,
		local_id INTEGER NOT NULL,
		account_email TEXT NOT NULL,
		plan TEXT NOT NULL,
		activated_at INTEGER NOT NULL,
		due_at INTEGER NOT NULL,
		expiry_estimated INTEGER NOT NULL DEFAULT 1,
		status TEXT NOT NULL DEFAULT 'pending',
		attempts INTEGER NOT NULL DEFAULT 0,
		channel TEXT NOT NULL DEFAULT '',
		last_error TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		sent_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_customer_expiry_notifications_due
		ON customer_expiry_notifications(status,due_at);
	UPDATE customer_expiry_notifications
		SET status='review',last_error='REMINDER_RESULT_UNCERTAIN',updated_at=strftime('%s','now')
		WHERE status='sending';`)
	return err
}

func customerExpiryEventID(email, plan string, activatedAt int64) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email)) + "\n" + plan + "\n" + fmt.Sprintf("%d", activatedAt)))
	return hex.EncodeToString(sum[:])
}

func seedCustomerExpiryNotifications(now int64) error {
	rows, err := db.DB.Query(operationsCustomerCurrentCTE + `SELECT id,email,plan,activated_at,
		subscription_expires_at,expiry_estimated FROM current_customers
		WHERE subscription_expires_at>?`, now)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item customerExpiryReminder
		var estimated int
		if err = rows.Scan(&item.LocalID, &item.AccountEmail, &item.Plan, &item.ActivatedAt, &item.DueAt, &estimated); err != nil {
			return err
		}
		item.AccountEmail = strings.ToLower(strings.TrimSpace(item.AccountEmail))
		if item.AccountEmail == "" || item.ActivatedAt <= 0 || item.DueAt <= item.ActivatedAt {
			continue
		}
		item.EventID = customerExpiryEventID(item.AccountEmail, item.Plan, item.ActivatedAt)
		item.ExpiryEstimated = estimated == 1
		estimatedValue := 0
		if item.ExpiryEstimated {
			estimatedValue = 1
		}
		_, err = db.DB.Exec(`INSERT INTO customer_expiry_notifications
			(event_id,local_id,account_email,plan,activated_at,due_at,expiry_estimated,status,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,'pending',?,?)
			ON CONFLICT(event_id) DO UPDATE SET
				local_id=excluded.local_id,account_email=excluded.account_email,plan=excluded.plan,
				due_at=excluded.due_at,expiry_estimated=excluded.expiry_estimated,updated_at=excluded.updated_at
			WHERE customer_expiry_notifications.status='pending'`, item.EventID, item.LocalID, item.AccountEmail,
			item.Plan, item.ActivatedAt, item.DueAt, estimatedValue, now, now)
		if err != nil {
			return err
		}
	}
	return rows.Err()
}

func dueCustomerExpiryNotifications(now int64, limit int) ([]customerExpiryReminder, error) {
	rows, err := db.DB.Query(`SELECT event_id,local_id,account_email,plan,activated_at,due_at,expiry_estimated
		FROM customer_expiry_notifications WHERE status='pending' AND due_at<=?
		ORDER BY due_at,event_id LIMIT ?`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []customerExpiryReminder{}
	for rows.Next() {
		var item customerExpiryReminder
		var estimated int
		if err = rows.Scan(&item.EventID, &item.LocalID, &item.AccountEmail, &item.Plan, &item.ActivatedAt, &item.DueAt, &estimated); err != nil {
			return nil, err
		}
		item.ExpiryEstimated = estimated == 1
		items = append(items, item)
	}
	return items, rows.Err()
}

func sendCustomerExpiryNotification(ctx context.Context, item customerExpiryReminder) (string, error) {
	identities, err := marketplaceCustomerIdentities(ctx, nil, []string{item.AccountEmail}, []marketplaceCustomerRecord{{
		AccountEmail: item.AccountEmail, ActivatedAt: item.ActivatedAt, Plan: item.Plan,
	}})
	identity := marketplaceCustomerIdentity{AccountEmail: item.AccountEmail, Source: "upgrade_email_inferred", Confidence: 35}
	if err == nil {
		if resolved, ok := identities["email:"+item.AccountEmail]; ok {
			identity = resolved
		}
	}
	payload, err := json.Marshal(map[string]any{
		"v": 1, "eventId": item.EventID, "accountEmail": item.AccountEmail,
		"buyerEmail": identity.BuyerEmail, "telegramId": identity.TelegramID,
		"identityConfidence": identity.Confidence, "identitySource": identity.Source,
		"plan": item.Plan, "dueAt": item.DueAt, "expiryEstimated": item.ExpiryEstimated,
	})
	if err != nil {
		return "", err
	}
	secret, err := completionBridgeSecret()
	if err != nil {
		return "", err
	}
	stamp := fmt.Sprintf("%d", time.Now().UnixMilli())
	url := strings.TrimSpace(os.Getenv("CDK_EXPIRY_REMINDER_URL"))
	if url == "" {
		url = "https://maple1189ai.com/api/internal/cdk-expiry-reminder"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MaplePass-Time", stamp)
	req.Header.Set("X-MaplePass-Signature", completionBridgeSignature(secret, "cdk-expiry-reminder:v1", stamp, payload))
	response, err := marketplaceCompletionClient.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, 32<<10))
	if readErr != nil || response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("CDK_EXPIRY_REMINDER_UNAVAILABLE")
	}
	var result struct {
		OK      bool   `json:"ok"`
		Channel string `json:"channel"`
	}
	if json.Unmarshal(raw, &result) != nil || !result.OK || (result.Channel != "EMAIL" && result.Channel != "TELEGRAM") {
		return "", fmt.Errorf("CDK_EXPIRY_REMINDER_UNCONFIRMED")
	}
	return result.Channel, nil
}

func processCustomerExpiryNotifications(ctx context.Context) {
	if !customerExpiryReminderMu.TryLock() {
		return
	}
	defer customerExpiryReminderMu.Unlock()
	now := time.Now().Unix()
	if err := seedCustomerExpiryNotifications(now); err != nil {
		return
	}
	items, err := dueCustomerExpiryNotifications(now, 20)
	if err != nil {
		return
	}
	for _, item := range items {
		result, err := db.DB.Exec(`UPDATE customer_expiry_notifications SET status='sending',attempts=attempts+1,
			updated_at=? WHERE event_id=? AND status='pending'`, now, item.EventID)
		if err != nil {
			continue
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			continue
		}
		channel, sendErr := sendCustomerExpiryNotification(ctx, item)
		finishedAt := time.Now().Unix()
		if sendErr != nil {
			_, _ = db.DB.Exec(`UPDATE customer_expiry_notifications SET status='review',last_error=?,updated_at=?
				WHERE event_id=? AND status='sending'`, "CDK_EXPIRY_REMINDER_FAILED", finishedAt, item.EventID)
			continue
		}
		_, _ = db.DB.Exec(`UPDATE customer_expiry_notifications SET status='sent',channel=?,last_error='',sent_at=?,updated_at=?
			WHERE event_id=? AND status='sending'`, channel, finishedAt, finishedAt, item.EventID)
	}
}

func StartCustomerExpiryNotifications(ctx context.Context) {
	go func() {
		timer := time.NewTimer(20 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			processCustomerExpiryNotifications(ctx)
		}
		ticker := time.NewTicker(customerExpiryReminderInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				processCustomerExpiryNotifications(ctx)
			}
		}
	}()
}

