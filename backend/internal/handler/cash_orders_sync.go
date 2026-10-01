package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type cashOrderSync struct {
	OrderNumber         string  `json:"order_number"`
	ProductKey          string  `json:"product_key"`
	ProductTitle        string  `json:"product_title"`
	Amount              float64 `json:"amount"`
	Currency            string  `json:"currency"`
	Status              string  `json:"status"`
	FulfillmentSource   string  `json:"fulfillment_source"`
	InventoryID         string  `json:"inventory_id"`
	CustomerTelegramID  string  `json:"customer_telegram_id"`
	CreatedAt           string  `json:"created_at"`
	UpdatedAt           string  `json:"updated_at"`
	DeliveredAt         string  `json:"delivered_at"`
	DeliveryReceipt     string  `json:"delivery_receipt"`
}

func cashOrderSyncURL() string {
	if value := strings.TrimSpace(os.Getenv("MAPLE_CASH_ORDERS_URL")); value != "" { return value }
	return "https://maple1189ai.com/api/internal/cdk-cash-orders"
}

func fetchCashOrders(ctx context.Context) ([]map[string]any, error) {
	endpoint := cashOrderSyncURL()
	secret := strings.TrimSpace(os.Getenv("CDK_SSO_SHARED_SECRET"))
	if endpoint == "" { return nil, nil }
	if len(secret) < 32 { return nil, errors.New("cash orders sync secret is not configured") }
	body := []byte(`{"v":1,"limit":1000}`)
	stamp := time.Now().UnixMilli()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("cdk-cash-orders:v1:" + formatInt64(stamp) + ":"))
	mac.Write(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil { return nil, err }
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MaplePass-Time", formatInt64(stamp))
	req.Header.Set("X-MaplePass-Signature", hex.EncodeToString(mac.Sum(nil)))
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil { return nil, err }
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK { return nil, errors.New("cash orders sync returned non-200") }
	limited := io.LimitReader(resp.Body, 2<<20)
	var decoded struct{ Orders []cashOrderSync `json:"orders"` }
	if err := json.NewDecoder(limited).Decode(&decoded); err != nil { return nil, err }
	rows := make([]map[string]any, 0, len(decoded.Orders))
	for _, row := range decoded.Orders {
		status := map[string]string{"DELIVERED":"completed", "DELIVERING":"running", "READY":"pending", "PENDING_CLAIM":"pending", "REVIEW":"requires_action", "EXPIRED":"cancelled", "CANCELLED":"cancelled"}[row.Status]
		if status == "" { status = "requires_action" }
		account := "现金订单"
		if row.CustomerTelegramID != "" { account += " · Telegram " + row.CustomerTelegramID }
		created := row.CreatedAt
		completed := row.DeliveredAt
		if completed == "" && row.Status == "DELIVERED" { completed = row.UpdatedAt }
		rows = append(rows, map[string]any{
			"id": row.OrderNumber, "client_request_id": row.OrderNumber, "account_email": account,
			"product": "cash", "plan": row.ProductKey, "product_title": row.ProductTitle,
			"status": status, "stage": row.Status, "currency": row.Currency,
			"final_amount_minor": int64(row.Amount * 100), "quoted_amount_minor": int64(row.Amount * 100),
			"created_at": created, "updated_at": row.UpdatedAt, "completed_at": completed,
			"synced_only": true, "source": "cash_delivery", "cash_order": true,
			"cash_fulfillment_source": row.FulfillmentSource, "cash_inventory_id": row.InventoryID,
			"cash_delivery_receipt": row.DeliveryReceipt,
		})
	}
	return rows, nil
}

func formatInt64(value int64) string {
	if value == 0 { return "0" }
	negative := value < 0
	if negative { value = -value }
	buf := make([]byte, 0, 20)
	for value > 0 { buf = append([]byte{byte('0' + value%10)}, buf...); value /= 10 }
	if len(buf) == 0 { buf = []byte{'0'} }
	if negative { buf = append([]byte{'-'}, buf...) }
	return string(buf)
}
