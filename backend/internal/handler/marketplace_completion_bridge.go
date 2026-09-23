package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

const (
	marketplaceCompletionBindPurpose     = "cdk-completion-bind:v1"
	marketplaceCompletionDeliveryPurpose = "cdk-completion:v1"
	defaultCompletionWebhookURL          = "https://maple1189ai.com/api/internal/cdk-completion"
	marketplaceCompletionLeaseSeconds    = int64(60)
	marketplaceCompletionBatchSize       = 5
)

var marketplaceCompletionClient = &http.Client{Timeout: 8 * time.Second}

var (
	errMarketplaceBindingNotFound    = errors.New("marketplace binding code not found")
	errMarketplaceBindingConflict    = errors.New("marketplace binding conflict")
	errMarketplaceCompletionConflict = errors.New("marketplace completion outbox conflict")
)

// marketplaceCompletionBindRequest deliberately carries only an irreversible
// code hash and the marketplace's opaque order UUID. Customer email, Telegram
// identifiers, credentials, and the plaintext CDK stay out of this service.
type marketplaceCompletionBindRequest struct {
	CodeHash string `json:"codeHash"`
	OrderID  string `json:"orderId"`
}

type marketplaceCompletionEvent struct {
	EventID     string `json:"eventId"`
	OrderID     string `json:"orderId"`
	LocalID     int64  `json:"localId"`
	CodeHash    string `json:"codeHash"`
	Plan        string `json:"plan"`
	CompletedAt string `json:"completedAt"`
}

type marketplaceCompletionOutboxRow struct {
	EventID       string
	OrderID       string
	CodeHash      string
	Plan          string
	LocalID       int64
	CompletedAt   int64
	Attempts      int
	NextAttemptAt int64
	LeaseUntil    int64
}

// MarketplaceLocalCDKBind accepts a signed, one-way association from the
// marketplace after it has delivered a paid native order. It is intentionally
// separate from public redemption endpoints and cannot overwrite an existing
// association with a different order.
func MarketplaceLocalCDKBind(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	raw, ok := verifyMarketplaceCompletionRequest(c, marketplaceCompletionBindPurpose)
	if !ok {
		return
	}
	var req marketplaceCompletionBindRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		localError(c, http.StatusBadRequest, "请求格式不正确")
		return
	}
	codeHash, validHash := canonicalCodeHash(req.CodeHash)
	orderID, validOrder := canonicalMarketplaceOrderID(req.OrderID)
	if !validHash || !validOrder {
		localError(c, http.StatusBadRequest, "请求参数不正确")
		return
	}
	localID, existing, err := bindMarketplaceLocalCDK(codeHash, orderID, time.Now().Unix())
	if errors.Is(err, errMarketplaceBindingNotFound) {
		localError(c, http.StatusNotFound, "兑换码不存在")
		return
	}
	if errors.Is(err, errMarketplaceBindingConflict) {
		localError(c, http.StatusConflict, "兑换码已绑定至其他订单")
		return
	}
	if err != nil {
		localError(c, http.StatusServiceUnavailable, "订单绑定暂时无法保存")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "localId": localID, "alreadyBound": existing})
}

func canonicalCodeHash(raw string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if len(v) != sha256.Size*2 {
		return "", false
	}
	if _, err := hex.DecodeString(v); err != nil {
		return "", false
	}
	return v, true
}

func canonicalMarketplaceOrderID(raw string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if len(v) != 36 {
		return "", false
	}
	for i := 0; i < len(v); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if v[i] != '-' {
				return "", false
			}
			continue
		}
		if !((v[i] >= '0' && v[i] <= '9') || (v[i] >= 'a' && v[i] <= 'f')) {
			return "", false
		}
	}
	return v, true
}

func completionBridgeSecret() (string, error) {
	secret := strings.TrimSpace(os.Getenv("CDK_SSO_SHARED_SECRET"))
	if len(secret) < 32 {
		return "", errors.New("completion bridge secret missing")
	}
	return secret, nil
}

func completionBridgeSignature(secret, purpose, stamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(purpose + ":" + stamp + ":"))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func verifyMarketplaceCompletionRequest(c *gin.Context, purpose string) ([]byte, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		localError(c, http.StatusBadRequest, "请求格式不正确")
		return nil, false
	}
	stamp := strings.TrimSpace(c.GetHeader("X-MaplePass-Time"))
	millis, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || millis <= 0 || absInt64(time.Now().UnixMilli()-millis) > int64(5*time.Minute/time.Millisecond) {
		c.Status(http.StatusUnauthorized)
		return nil, false
	}
	secret, err := completionBridgeSecret()
	if err != nil {
		c.Status(http.StatusServiceUnavailable)
		return nil, false
	}
	expected := completionBridgeSignature(secret, purpose, stamp, raw)
	got := strings.ToLower(strings.TrimSpace(c.GetHeader("X-MaplePass-Signature")))
	if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		c.Status(http.StatusUnauthorized)
		return nil, false
	}
	return raw, true
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

// bindMarketplaceLocalCDK records a one-way local-code -> marketplace-order
// association. Repeating the same association is harmless; any attempt to
// replace it with a different order is rejected.
func bindMarketplaceLocalCDK(codeHash, orderID string, now int64) (localID int64, alreadyBound bool, err error) {
	tx, err := db.DB.Begin()
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	var storedHash, plan, status, upstreamCompletionSource string
	var activatedAt, upstreamCompletionVerifiedAt int64
	if err = tx.QueryRow(`SELECT id,code_hash,plan,status,activated_at,upstream_completion_verified_at,upstream_completion_source FROM local_cdks WHERE code_hash=?`, codeHash).
		Scan(&localID, &storedHash, &plan, &status, &activatedAt, &upstreamCompletionVerifiedAt, &upstreamCompletionSource); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, errMarketplaceBindingNotFound
		}
		return 0, false, err
	}

	var existingOrder string
	err = tx.QueryRow(`SELECT marketplace_order_id FROM marketplace_local_cdk_bindings WHERE local_id=?`, localID).Scan(&existingOrder)
	switch {
	case err == nil:
		if existingOrder != orderID {
			return 0, false, errMarketplaceBindingConflict
		}
		alreadyBound = true
	case errors.Is(err, sql.ErrNoRows):
		// A paid marketplace order is one delivery. Do not allow a retry,
		// operator mistake, or race to associate it with a second local code.
		var orderLocalID int64
		orderErr := tx.QueryRow(`SELECT local_id FROM marketplace_local_cdk_bindings WHERE marketplace_order_id=?`, orderID).Scan(&orderLocalID)
		if orderErr == nil && orderLocalID != localID {
			return 0, false, errMarketplaceBindingConflict
		}
		if orderErr != nil && !errors.Is(orderErr, sql.ErrNoRows) {
			return 0, false, orderErr
		}
		result, insertErr := tx.Exec(`INSERT OR IGNORE INTO marketplace_local_cdk_bindings
			(local_id,code_hash,marketplace_order_id,created_at) VALUES(?,?,?,?)`, localID, storedHash, orderID, now)
		if insertErr != nil {
			return 0, false, insertErr
		}
		if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
			// Another request may have won the race. Read the local association
			// back and accept only an identical association; otherwise distinguish
			// the duplicate order from a transient storage fault.
			if readErr := tx.QueryRow(`SELECT marketplace_order_id FROM marketplace_local_cdk_bindings WHERE local_id=?`, localID).Scan(&existingOrder); readErr != nil {
				if errors.Is(readErr, sql.ErrNoRows) {
					var boundLocalID int64
					if lookupErr := tx.QueryRow(`SELECT local_id FROM marketplace_local_cdk_bindings WHERE marketplace_order_id=?`, orderID).Scan(&boundLocalID); lookupErr == nil && boundLocalID != localID {
						return 0, false, errMarketplaceBindingConflict
					}
				}
				return 0, false, readErr
			}
			if existingOrder != orderID {
				return 0, false, errMarketplaceBindingConflict
			}
			alreadyBound = true
		}
	default:
		return 0, false, err
	}

	// A customer can redeem immediately after delivery. If completion won that
	// race, create the same durable event while recording the late binding — but
	// only if the consumed state has a proof written by the authoritative
	// direct-order check. Historical/imported `consumed` rows cannot be made
	// invoice-eligible by binding them after the fact.
	if status == "consumed" {
		if upstreamCompletionVerifiedAt <= 0 || upstreamCompletionSource != "cardplatform_direct_order" {
			return 0, false, errMarketplaceBindingConflict
		}
		if activatedAt <= 0 {
			if err = recordLocalCompletionDetailsTx(tx, localID, plan, "", upstreamCompletionVerifiedAt); err != nil {
				return 0, false, err
			}
			activatedAt = upstreamCompletionVerifiedAt
		}
		if err = enqueueMarketplaceCompletionTx(tx, localID, storedHash, plan, orderID, activatedAt, now); err != nil {
			return 0, false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, false, err
	}
	return localID, alreadyBound, nil
}

// enqueueMarketplaceCompletionTx is called in the same SQLite transaction as
// the authoritative consumed transition. No customer identity or plaintext CDK
// is ever persisted in the outbox.
func enqueueMarketplaceCompletionTx(tx *sql.Tx, localID int64, codeHash, plan, orderID string, completedAt, now int64) error {
	if completedAt <= 0 {
		completedAt = now
	}
	eventID := fmt.Sprintf("local:%d:completed", localID)
	result, err := tx.Exec(`INSERT OR IGNORE INTO marketplace_completion_outbox
		(event_id,local_id,code_hash,marketplace_order_id,plan,completed_at,state,attempts,next_attempt_at,lease_until,last_error,created_at,updated_at)
		VALUES(?,?,?,?,?,?, 'pending',0,?,0,'',?,?)`,
		eventID, localID, codeHash, orderID, plan, completedAt, now, now, now)
	if err != nil {
		return err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr == nil && changed == 1 {
		return nil
	}
	var existingHash, existingOrder, existingPlan string
	var existingLocalID int64
	err = tx.QueryRow(`SELECT local_id,code_hash,marketplace_order_id,plan FROM marketplace_completion_outbox WHERE event_id=?`, eventID).
		Scan(&existingLocalID, &existingHash, &existingOrder, &existingPlan)
	if err != nil {
		return err
	}
	if existingLocalID != localID || existingHash != codeHash || existingOrder != orderID || existingPlan != plan {
		return errMarketplaceCompletionConflict
	}
	return nil
}

func enqueueMarketplaceCompletionForBoundLocalTx(tx *sql.Tx, localID int64, codeHash, plan string, completedAt, now int64) error {
	var orderID string
	err := tx.QueryRow(`SELECT marketplace_order_id FROM marketplace_local_cdk_bindings WHERE local_id=?`, localID).Scan(&orderID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return enqueueMarketplaceCompletionTx(tx, localID, codeHash, plan, orderID, completedAt, now)
}

func completionWebhookURL() string {
	if endpoint := strings.TrimSpace(os.Getenv("MAPLE_STORE_COMPLETION_WEBHOOK_URL")); endpoint != "" {
		return endpoint
	}
	return defaultCompletionWebhookURL
}

func postMarketplaceCompletion(ctx context.Context, event marketplaceCompletionEvent) error {
	secret, err := completionBridgeSecret()
	if err != nil {
		return err
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, completionWebhookURL(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MaplePass-Time", stamp)
	req.Header.Set("X-MaplePass-Signature", completionBridgeSignature(secret, marketplaceCompletionDeliveryPurpose, stamp, body))
	response, err := marketplaceCompletionClient.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("completion bridge status %d", response.StatusCode)
	}
	return nil
}

func claimMarketplaceCompletionOutbox(now int64) (*marketplaceCompletionOutboxRow, error) {
	tx, err := db.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	row := &marketplaceCompletionOutboxRow{}
	err = tx.QueryRow(`SELECT event_id,local_id,code_hash,marketplace_order_id,plan,completed_at,attempts,next_attempt_at,lease_until
		FROM marketplace_completion_outbox
		WHERE (state='pending' AND next_attempt_at<=?) OR (state='sending' AND lease_until<=?)
		ORDER BY next_attempt_at,event_id LIMIT 1`, now, now).
		Scan(&row.EventID, &row.LocalID, &row.CodeHash, &row.OrderID, &row.Plan, &row.CompletedAt, &row.Attempts, &row.NextAttemptAt, &row.LeaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result, err := tx.Exec(`UPDATE marketplace_completion_outbox
		SET state='sending',attempts=attempts+1,lease_until=?,updated_at=?
		WHERE event_id=? AND ((state='pending' AND next_attempt_at<=?) OR (state='sending' AND lease_until<=?))`,
		now+marketplaceCompletionLeaseSeconds, now, row.EventID, now, now)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return nil, nil
	}
	row.Attempts++
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return row, nil
}

func completionRetryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	shift := attempts - 1
	if shift > 5 {
		shift = 5
	}
	delay := 30 * time.Second * time.Duration(1<<shift)
	if delay > 15*time.Minute {
		return 15 * time.Minute
	}
	return delay
}

func releaseMarketplaceCompletionOutbox(eventID string, attempts int, now int64) {
	next := now + int64(completionRetryDelay(attempts)/time.Second)
	_, _ = db.DB.Exec(`UPDATE marketplace_completion_outbox
		SET state='pending',next_attempt_at=?,lease_until=0,last_error='delivery_failed',updated_at=?
		WHERE event_id=? AND state='sending'`, next, now, eventID)
}

func markMarketplaceCompletionDelivered(eventID string, now int64) error {
	result, err := db.DB.Exec(`UPDATE marketplace_completion_outbox
		SET state='sent',lease_until=0,last_error='',sent_at=?,updated_at=?
		WHERE event_id=? AND state='sending'`, now, now, eventID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return errors.New("completion outbox acknowledgement lost")
	}
	return nil
}

func marketplaceCompletionAlertKey(eventID string) string {
	return "marketplace_completion:" + eventID
}

// dispatchMarketplaceCompletionOutbox is invoked by the existing automation
// cycle. The lease makes crash recovery safe; the marketplace must deduplicate
// eventId because a network timeout after its acceptance is inherently unknown.
func dispatchMarketplaceCompletionOutbox(ctx context.Context) {
	for i := 0; i < marketplaceCompletionBatchSize && ctx.Err() == nil; i++ {
		now := time.Now().Unix()
		row, err := claimMarketplaceCompletionOutbox(now)
		if err != nil {
			autoAlert("marketplace_completion_outbox", 0, "商城升级成功通知队列暂时无法读取，系统将继续重试。")
			return
		}
		if row == nil {
			return
		}
		event := marketplaceCompletionEvent{
			EventID:     row.EventID,
			OrderID:     row.OrderID,
			LocalID:     row.LocalID,
			CodeHash:    row.CodeHash,
			Plan:        row.Plan,
			CompletedAt: time.Unix(row.CompletedAt, 0).UTC().Format(time.RFC3339),
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		err = postMarketplaceCompletion(attemptCtx, event)
		cancel()
		if err != nil {
			releaseMarketplaceCompletionOutbox(row.EventID, row.Attempts, now)
			autoAlert(marketplaceCompletionAlertKey(row.EventID), row.LocalID, "商城升级成功通知暂未送达，系统将继续重试。")
			return
		}
		if err = markMarketplaceCompletionDelivered(row.EventID, now); err != nil {
			autoAlert(marketplaceCompletionAlertKey(row.EventID), row.LocalID, "商城升级成功通知已提交但回执暂未保存，系统将安全重试。")
			return
		}
		autoResolve(marketplaceCompletionAlertKey(row.EventID))
	}
}
