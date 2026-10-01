// Package provider defines the stable boundary between business workflows and
// external card/top-up suppliers. Existing suppliers can be wrapped
// incrementally; this package deliberately contains no network or database
// code.
package provider

import (
	"context"
	"errors"
)

// Capability describes optional operations supported by a supplier.
type Capability string

const (
	CapabilityCards       Capability = "cards"
	CapabilityTopUp       Capability = "topup"
	CapabilityCDK         Capability = "cdk"
	CapabilityWebhook     Capability = "webhook"
	CapabilityTransactions Capability = "transactions"
)

// Health is safe to expose in an operations dashboard; it must not contain
// API keys or raw provider responses.
type Health struct {
	Provider string
	Healthy  bool
	Message  string
}

// OrderRequest is intentionally provider-neutral. Product-specific fields
// belong in Metadata so the business order remains the source of truth.
type OrderRequest struct {
	Reference string
	Product   string
	AmountMinor int64
	Currency    string
	Metadata    map[string]string
}

type OrderResult struct {
	ExternalID string
	Status     string
	RawRef     string
}

// Provider is the minimum contract for a new upstream adapter. Optional
// capabilities should be advertised by Capabilities and return
// ErrUnsupported when called without support.
type Provider interface {
	Name() string
	Capabilities() []Capability
	HealthCheck(context.Context) Health
	CreateOrder(context.Context, OrderRequest) (OrderResult, error)
	GetOrder(context.Context, string) (OrderResult, error)
}

// WebhookConsumer is implemented by providers that can verify and normalize
// callback payloads. The HTTP handler remains responsible for signature,
// timestamp and replay checks before calling this boundary.
type WebhookConsumer interface {
	Provider
	NormalizeWebhook(context.Context, []byte, map[string]string) (WebhookEvent, error)
}

type WebhookEvent struct {
	EventID    string
	ExternalID string
	Type       string
	Status     string
	OccurredAt int64
	Payload    map[string]any
}

// ErrUnsupported is returned by adapters for capabilities they do not expose.
var ErrUnsupported = errors.New("provider operation is not supported")
