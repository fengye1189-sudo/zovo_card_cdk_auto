package cardplatform

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/provider"
)

// ProviderAdapter exposes the existing Zovo client through the shared
// provider contract. It is an opt-in wrapper; current handlers continue to
// call Client directly until routing is migrated deliberately.
type ProviderAdapter struct{ client *Client }

func NewProviderAdapter(client *Client) *ProviderAdapter {
	if client == nil {
		client = NewFromSettings()
	}
	return &ProviderAdapter{client: client}
}

func (a *ProviderAdapter) Name() string { return "zovo" }

func (a *ProviderAdapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityCards, provider.CapabilityTopUp, provider.CapabilityCDK, provider.CapabilityWebhook, provider.CapabilityTransactions}
}

func (a *ProviderAdapter) HealthCheck(ctx context.Context) provider.Health {
	if a == nil || a.client == nil {
		return provider.Health{Provider: "zovo", Message: "client is not configured"}
	}
	if _, err := a.client.GetBalance(ctx); err != nil {
		return provider.Health{Provider: "zovo", Message: err.Error()}
	}
	return provider.Health{Provider: "zovo", Healthy: true, Message: "ok"}
}

func (a *ProviderAdapter) CreateOrder(ctx context.Context, req provider.OrderRequest) (provider.OrderResult, error) {
	if a == nil || a.client == nil {
		return provider.OrderResult{}, fmt.Errorf("zovo client is not configured")
	}
	body := map[string]any{
		"client_request_id": req.Reference,
		"product":           req.Product,
		"amount_minor":      req.AmountMinor,
		"currency":          req.Currency,
	}
	for k, v := range req.Metadata {
		body[k] = v
	}
	raw, err := a.client.DirectOrder(ctx, body, req.Reference)
	if err != nil {
		return provider.OrderResult{}, err
	}
	return normalizeOrderResult(raw), nil
}

func (a *ProviderAdapter) GetOrder(ctx context.Context, externalID string) (provider.OrderResult, error) {
	if a == nil || a.client == nil {
		return provider.OrderResult{}, fmt.Errorf("zovo client is not configured")
	}
	id, err := strconv.ParseInt(strings.TrimSpace(externalID), 10, 64)
	if err != nil || id <= 0 {
		return provider.OrderResult{}, fmt.Errorf("invalid zovo order id")
	}
	raw, err := a.client.DirectOrderStatus(ctx, id)
	if err != nil {
		return provider.OrderResult{}, err
	}
	return normalizeOrderResult(raw), nil
}

// Quote exposes the authoritative service fee published by ZOVO. Product
// names are normalized here so routing code does not import ZOVO plan keys.
func (a *ProviderAdapter) Quote(ctx context.Context, product string) (provider.Quote, error) {
	if a == nil || a.client == nil {
		return provider.Quote{}, fmt.Errorf("zovo client is not configured")
	}
	plan := strings.TrimSpace(product)
	if plan == "" {
		plan = "plus"
	}
	_, fee, err := a.client.DirectPricing(ctx, plan)
	if err != nil {
		return provider.Quote{Provider: a.Name(), Product: plan, Available: false, Currency: "USD"}, err
	}
	stock, stockErr := a.Inventory(ctx, plan)
	return provider.Quote{
		Provider: a.Name(), Product: plan, Available: stockErr == nil && stock > 0,
		CostMinor: fee, Currency: "USD", Stock: stock, ObservedAt: time.Now().Unix(),
	}, stockErr
}

func (a *ProviderAdapter) Inventory(ctx context.Context, product string) (int64, error) {
	if a == nil || a.client == nil {
		return 0, fmt.Errorf("zovo client is not configured")
	}
	candidates, err := a.client.DirectCandidatesForPlan(ctx, strings.TrimSpace(product))
	if err != nil {
		return 0, err
	}
	var available int64
	for _, candidate := range candidates {
		if candidate.Usable(1) {
			available++
		}
	}
	return available, nil
}

func normalizeOrderResult(raw json.RawMessage) provider.OrderResult {
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		return provider.OrderResult{RawRef: string(raw)}
	}
	return provider.OrderResult{
		ExternalID: firstString(v, "id", "order_id", "external_id"),
		Status:     firstString(v, "status", "order_status", "stage"),
		RawRef:     string(raw),
	}
}

func firstString(v map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := v[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
		if n, ok := v[k].(float64); ok {
			return strconv.FormatInt(int64(n), 10)
		}
	}
	return ""
}
