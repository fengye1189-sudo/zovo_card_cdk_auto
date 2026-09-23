package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/db"
)

type marketplaceFailureEvent struct {
	Version        int    `json:"v"`
	EventID        string `json:"eventId"`
	LocalID        int64  `json:"localId"`
	CodePrefix     string `json:"codePrefix"`
	Product        string `json:"product"`
	UpstreamOrder  int64  `json:"upstreamOrder"`
	UpstreamStatus string `json:"upstreamStatus"`
	Permanent      bool   `json:"permanent"`
}

var marketplaceFailureClient = &http.Client{Timeout: 8 * time.Second}

func postMarketplaceFailure(ctx context.Context, event marketplaceFailureEvent) error {
	secret := strings.TrimSpace(os.Getenv("CDK_SSO_SHARED_SECRET"))
	if len(secret) < 32 {
		return errors.New("failure bridge secret missing")
	}
	endpoint := strings.TrimSpace(os.Getenv("MAPLE_STORE_FAILURE_WEBHOOK_URL"))
	if endpoint == "" {
		endpoint = "https://store.maple1189ai.com/api/internal/cdk-failure"
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("cdk-failure:v1:" + stamp + ":" + string(body)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MaplePass-Time", stamp)
	req.Header.Set("X-MaplePass-Signature", hex.EncodeToString(mac.Sum(nil)))
	response, err := marketplaceFailureClient.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("failure bridge status %d", response.StatusCode)
	}
	return nil
}

func claimMarketplaceFailure(key string, localID int64, eventID string) bool {
	now := time.Now().Unix()
	pending := "marketplace_bridge_pending:" + eventID
	done := "marketplace_bridge_done:" + eventID
	result, err := db.DB.Exec(`INSERT INTO automation_alerts(alert_key,local_id,message,updated_at,resolved)
		VALUES(?,?,?,?,0) ON CONFLICT(alert_key) DO NOTHING`, key, localID, pending, now)
	if err == nil {
		if count, countErr := result.RowsAffected(); countErr == nil && count == 1 {
			return true
		}
	}
	// A five-minute lease recovers a process that stopped after claiming but
	// before learning whether the signed request was accepted. Customer delivery
	// is independently idempotent, so this cannot issue a CDK twice.
	result, err = db.DB.Exec(`UPDATE automation_alerts SET local_id=?,message=?,updated_at=?,resolved=0
		WHERE alert_key=? AND message<>? AND (message<>? OR updated_at<?)`,
		localID, pending, now, key, done, pending, now-300)
	if err != nil {
		return false
	}
	count, err := result.RowsAffected()
	return err == nil && count == 1
}

func notifyLocalFailure(ctx context.Context, key string, r localCode, upstreamStatus string, permanent bool) {
	product, prefix := r.Plan, ""
	_ = db.DB.QueryRow(`SELECT COALESCE(p.name,c.plan),c.prefix
		FROM local_cdks c
		LEFT JOIN operations_product_bindings b ON b.local_id=c.id
		LEFT JOIN operations_products p ON p.id=COALESCE(b.product_id,'plus')
		WHERE c.id=?`, r.ID).Scan(&product, &prefix)
	event := marketplaceFailureEvent{Version: 1, EventID: fmt.Sprintf("local:%d:%s", r.ID, upstreamStatus), LocalID: r.ID,
		CodePrefix: prefix, Product: product, UpstreamOrder: r.Upstream, UpstreamStatus: upstreamStatus, Permanent: permanent}
	if !claimMarketplaceFailure(key, r.ID, event.EventID) {
		return
	}
	bridgeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := postMarketplaceFailure(bridgeCtx, event); err != nil {
		autoAlert(key, r.ID, localFailureAdminMessage(r, upstreamStatus, permanent))
		return
	}
	// The MaplePass store bot accepted the detailed alert and customer-notice job.
	// Persist the exact event marker so later upstream checks do not alert again.
	_, _ = db.DB.Exec("UPDATE automation_alerts SET message=?,updated_at=?,resolved=1 WHERE alert_key=?",
		"marketplace_bridge_done:"+event.EventID, time.Now().Unix(), key)
}
