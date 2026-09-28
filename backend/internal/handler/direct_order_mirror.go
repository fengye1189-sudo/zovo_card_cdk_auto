package handler

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

const directOrderMirrorInterval = 2 * time.Minute

func mirrorString(order map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := order[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func mirrorInt(order map[string]any, key string) int64 { return anyToInt64(order[key]) }

func mirrorTime(value any) int64 {
	switch raw := value.(type) {
	case float64:
		if raw > 1e12 {
			return int64(raw / 1000)
		}
		return int64(raw)
	case int64:
		if raw > 1e12 {
			return raw / 1000
		}
		return raw
	case string:
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return 0
		}
		if number, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if number > 1e12 {
				return number / 1000
			}
			return number
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, raw); err == nil {
				return parsed.Unix()
			}
		}
	}
	return 0
}

func mirrorEmail(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if !strings.Contains(value, "@") || strings.Contains(value, "*") || strings.Contains(value, "[已隐藏]") {
		return ""
	}
	return value
}

func mirrorCompleted(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "done", "success", "succeeded":
		return true
	default:
		return false
	}
}

// mirrorDirectOrder stores only public fields returned by PublicDirectOrder.
// It is reconciliation bookkeeping and never triggers an upstream mutation.
func mirrorDirectOrder(order map[string]any, source string, observedAt int64) error {
	id := mirrorInt(order, "id")
	if id <= 0 {
		return nil
	}
	status := strings.ToLower(mirrorString(order, "status"))
	createdAt := mirrorTime(order["created_at"])
	updatedAt := mirrorTime(order["updated_at"])
	completedAt := mirrorTime(order["completed_at"])
	activatedAt, expiresAt, estimated := int64(0), int64(0), 0
	if mirrorCompleted(status) {
		activatedAt = completedAt
		if activatedAt <= 0 {
			activatedAt = updatedAt
		}
		if activatedAt <= 0 {
			activatedAt = createdAt
		}
		if activatedAt > 0 {
			expiresAt = time.Unix(activatedAt, 0).UTC().AddDate(0, 1, 0).Unix()
			estimated = 1
		}
	}
	_, err := db.DB.Exec(`INSERT INTO zovo_direct_order_mirror(
	 upstream_id,client_request_id,account_email,product,plan,status,stage,card_id,card_last_four,
	 currency,quoted_amount_minor,final_amount_minor,renewal_status,created_at,updated_at,completed_at,
	 activated_at,subscription_expires_at,expiry_estimated,source,synced_at)
	 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	 ON CONFLICT(upstream_id) DO UPDATE SET
	 client_request_id=CASE WHEN excluded.client_request_id<>'' THEN excluded.client_request_id ELSE zovo_direct_order_mirror.client_request_id END,
	 account_email=CASE WHEN excluded.account_email<>'' THEN excluded.account_email ELSE zovo_direct_order_mirror.account_email END,
	 product=CASE WHEN excluded.product<>'' THEN excluded.product ELSE zovo_direct_order_mirror.product END,
	 plan=CASE WHEN excluded.plan<>'' THEN excluded.plan ELSE zovo_direct_order_mirror.plan END,
	 status=CASE WHEN excluded.status<>'' THEN excluded.status ELSE zovo_direct_order_mirror.status END,
	 stage=CASE WHEN excluded.stage<>'' THEN excluded.stage ELSE zovo_direct_order_mirror.stage END,
	 card_id=CASE WHEN excluded.card_id>0 THEN excluded.card_id ELSE zovo_direct_order_mirror.card_id END,
	 card_last_four=CASE WHEN excluded.card_last_four<>'' THEN excluded.card_last_four ELSE zovo_direct_order_mirror.card_last_four END,
	 currency=CASE WHEN excluded.currency<>'' THEN excluded.currency ELSE zovo_direct_order_mirror.currency END,
	 quoted_amount_minor=CASE WHEN excluded.quoted_amount_minor>0 THEN excluded.quoted_amount_minor ELSE zovo_direct_order_mirror.quoted_amount_minor END,
	 final_amount_minor=CASE WHEN excluded.final_amount_minor>0 THEN excluded.final_amount_minor ELSE zovo_direct_order_mirror.final_amount_minor END,
	 renewal_status=CASE WHEN excluded.renewal_status<>'' THEN excluded.renewal_status ELSE zovo_direct_order_mirror.renewal_status END,
	 created_at=CASE WHEN excluded.created_at>0 THEN excluded.created_at ELSE zovo_direct_order_mirror.created_at END,
	 updated_at=MAX(zovo_direct_order_mirror.updated_at,excluded.updated_at),
	 completed_at=MAX(zovo_direct_order_mirror.completed_at,excluded.completed_at),
	 activated_at=CASE WHEN excluded.activated_at>0 THEN excluded.activated_at ELSE zovo_direct_order_mirror.activated_at END,
	 subscription_expires_at=CASE WHEN excluded.subscription_expires_at>0 THEN excluded.subscription_expires_at ELSE zovo_direct_order_mirror.subscription_expires_at END,
	 expiry_estimated=MAX(zovo_direct_order_mirror.expiry_estimated,excluded.expiry_estimated),
	 source=CASE WHEN excluded.source='zovo_api' OR zovo_direct_order_mirror.source='' THEN excluded.source ELSE zovo_direct_order_mirror.source END,
	 synced_at=excluded.synced_at`,
		id, mirrorString(order, "client_request_id"), mirrorEmail(mirrorString(order, "account_email", "email")),
		mirrorString(order, "product"), mirrorString(order, "plan"), status, mirrorString(order, "stage"),
		mirrorInt(order, "card_id"), mirrorString(order, "card_last_four"), mirrorString(order, "currency"),
		mirrorInt(order, "quoted_amount_minor"), mirrorInt(order, "final_amount_minor"), mirrorString(order, "renewal_status"),
		createdAt, updatedAt, completedAt, activatedAt, expiresAt, estimated, source, observedAt)
	return err
}

func syncWebhookDirectOrders(observedAt int64) error {
	events, err := db.ListDirectOrderWebhookEvents(2000)
	if err != nil {
		return err
	}
	for _, event := range events {
		var payload map[string]any
		if json.Unmarshal([]byte(event.Payload), &payload) != nil {
			continue
		}
		id := anyToInt64(payload["order_id"])
		if id <= 0 {
			id = anyToInt64(payload["id"])
		}
		if id <= 0 {
			continue
		}
		payload["id"] = float64(id)
		if _, ok := payload["status"]; !ok && strings.EqualFold(event.EventType, "gpt_direct.completed") {
			payload["status"] = "completed"
		}
		if _, ok := payload["created_at"]; !ok {
			payload["created_at"] = event.CreatedAt
		}
		if err := mirrorDirectOrder(cardplatform.PublicDirectOrder(payload), "zovo_webhook", observedAt); err != nil {
			return err
		}
	}
	return nil
}

func syncAPIDirectOrders(ctx context.Context, observedAt int64) error {
	client := cardplatform.NewFromSettings()
	for page := 1; page <= 100; page++ {
		data, err := client.DirectOrderList(ctx, page)
		if err != nil {
			return err
		}
		list, ok := data["list"].([]map[string]any)
		if !ok {
			return nil
		}
		for _, order := range list {
			if err := mirrorDirectOrder(order, "zovo_api", observedAt); err != nil {
				return err
			}
		}
		total := anyToInt64(data["total"])
		if len(list) < 20 || int64(page*20) >= total {
			return nil
		}
	}
	return nil
}

func syncDirectOrderMirror(ctx context.Context) {
	now := time.Now().Unix()
	if err := syncWebhookDirectOrders(now); err != nil {
		log.Printf("direct order webhook mirror: %v", err)
	}
	readCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := syncAPIDirectOrders(readCtx, now); err != nil {
		log.Printf("direct order api mirror: %v", err)
	}
}

// StartDirectOrderMirror keeps operations data current without a browser.
// The worker performs GETs plus local idempotent upserts only.
func StartDirectOrderMirror(ctx context.Context) {
	go func() {
		syncDirectOrderMirror(ctx)
		ticker := time.NewTicker(directOrderMirrorInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				syncDirectOrderMirror(ctx)
			}
		}
	}()
}
