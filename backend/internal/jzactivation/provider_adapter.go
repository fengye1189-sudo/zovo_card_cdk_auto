package jzactivation

import (
	"context"
	"fmt"
	"strings"

	"github.com/tuzi/cdk-recharge-system/internal/provider"
)

// ProviderAdapter exposes JZ activation as a provider without changing the
// existing managed-activation handlers. JZ is task-oriented rather than a
// generic card/order API, so unsupported operations remain explicit.
type ProviderAdapter struct{ client *Client }

func NewProviderAdapter(client *Client) *ProviderAdapter {
	if client == nil {
		client = NewFromEnv()
	}
	return &ProviderAdapter{client: client}
}

func (a *ProviderAdapter) Name() string { return "jzactivation" }

func (a *ProviderAdapter) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapabilityCDK, provider.CapabilityWebhook}
}

func (a *ProviderAdapter) HealthCheck(_ context.Context) provider.Health {
	if a == nil || a.client == nil {
		return provider.Health{Provider: "jzactivation", Message: "client is not configured"}
	}
	if !a.client.cfg.Enabled {
		return provider.Health{Provider: "jzactivation", Message: "channel disabled"}
	}
	if strings.TrimSpace(a.client.cfg.BaseURL) == "" {
		return provider.Health{Provider: "jzactivation", Message: "base URL is not configured"}
	}
	return provider.Health{Provider: "jzactivation", Healthy: true, Message: "configured"}
}

func (a *ProviderAdapter) CreateOrder(ctx context.Context, req provider.OrderRequest) (provider.OrderResult, error) {
	if a == nil || a.client == nil {
		return provider.OrderResult{}, fmt.Errorf("jz activation client is not configured")
	}
	code := strings.TrimSpace(req.Metadata["cdk_code"])
	session := strings.TrimSpace(req.Metadata["session_json"])
	if code == "" || session == "" {
		return provider.OrderResult{}, fmt.Errorf("jz activation requires cdk_code and session_json metadata")
	}
	result, _, err := a.client.CreateTask(ctx, code, session)
	if err != nil {
		return provider.OrderResult{}, err
	}
	return provider.OrderResult{ExternalID: result.TaskID, Status: result.Status}, nil
}

func (a *ProviderAdapter) GetOrder(context.Context, string) (provider.OrderResult, error) {
	return provider.OrderResult{}, provider.ErrUnsupported
}
