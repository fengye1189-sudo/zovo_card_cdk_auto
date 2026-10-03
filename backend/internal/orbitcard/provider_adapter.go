package orbitcard

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/tuzi/cdk-recharge-system/internal/provider"
)

func (e *APIError) FailureClass() provider.FailureClass {
	if e == nil {
		return provider.FailureUnknown
	}
	switch {
	case e.HTTPStatus == 429:
		return provider.FailureRateLimited
	case e.HTTPStatus == 401 || e.HTTPStatus == 403:
		return provider.FailureConfig
	case e.HTTPStatus >= 500 || e.HTTPStatus == 0:
		return provider.FailureTransient
	case e.HTTPStatus >= 400:
		return provider.FailureRejected
	default:
		return provider.FailureUnknown
	}
}

func (e *APIError) FailureRetryAfter() int { return 0 }

type ProviderAdapter struct{ client *Client }

func NewProviderAdapter(client *Client) *ProviderAdapter {
	if client == nil {
		client = NewFromEnv()
	}
	return &ProviderAdapter{client: client}
}

func (a *ProviderAdapter) Name() string { return "orbitcard" }
func (a *ProviderAdapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityTopUp}
}

func (a *ProviderAdapter) HealthCheck(ctx context.Context) provider.Health {
	if a == nil || a.client == nil {
		return provider.Health{Provider: "orbitcard", Message: "client is not configured"}
	}
	if !a.client.cfg.Enabled {
		return provider.Health{Provider: "orbitcard", Message: "disabled"}
	}
	if _, err := a.client.Plans(ctx); err != nil {
		return provider.Health{Provider: "orbitcard", Message: err.Error()}
	}
	return provider.Health{Provider: "orbitcard", Healthy: true, Message: "ok"}
}

func (a *ProviderAdapter) CreateOrder(ctx context.Context, req provider.OrderRequest) (provider.OrderResult, error) {
	if a == nil || a.client == nil {
		return provider.OrderResult{}, fmt.Errorf("orbitcard client is not configured")
	}
	plan := strings.TrimSpace(req.Metadata["plan_type"])
	if plan == "" {
		plan = req.Product
	}
	if len(req.Reference) < 8 {
		return provider.OrderResult{}, fmt.Errorf("orbitcard client_order_no is too short")
	}
	body := map[string]any{"client_order_no": req.Reference, "plan_type": plan, "authorized": true}
	if v := strings.TrimSpace(req.Metadata["payment_region"]); v != "" {
		body["payment_region"] = v
	}
	if v := strings.TrimSpace(req.Metadata["card_id"]); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return provider.OrderResult{}, fmt.Errorf("invalid orbitcard card_id")
		}
		body["card_id"] = id
	}
	if v := strings.TrimSpace(req.Metadata["session_json"]); v != "" {
		var session any
		if err := json.Unmarshal([]byte(v), &session); err != nil {
			return provider.OrderResult{}, fmt.Errorf("invalid orbitcard session_json")
		}
		body["session_json"] = session
	}
	if v := strings.TrimSpace(req.Metadata["max_service_fee"]); v != "" {
		body["max_service_fee"] = v
	}
	raw, err := a.client.CreateSubscription(ctx, body, req.Reference)
	if err != nil {
		return provider.OrderResult{}, err
	}
	return normalizeResult(raw), nil
}

func (a *ProviderAdapter) GetOrder(ctx context.Context, externalID string) (provider.OrderResult, error) {
	raw, err := a.client.GetSubscription(ctx, map[string]string{"task_no": strings.TrimSpace(externalID)})
	if err != nil {
		return provider.OrderResult{}, err
	}
	return normalizeResult(raw), nil
}

func normalizeResult(raw json.RawMessage) provider.OrderResult {
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		return provider.OrderResult{Status: "UNKNOWN", RawRef: string(raw)}
	}
	id := firstString(v, "task_no", "orderId", "client_order_no")
	status := strings.ToLower(firstString(v, "upstream_status", "status"))
	if status == "" {
		status = "unknown"
	}
	return provider.OrderResult{ExternalID: id, Status: status, RawRef: string(raw)}
}

func firstString(v map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := v[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// IsTerminalFailure intentionally excludes processing, captcha, timeout and
// unknown states. The subscription router must query those states instead of
// switching providers.
func IsTerminalFailure(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "declined", "rejected", "unsupported":
		return true
	default:
		return false
	}
}
