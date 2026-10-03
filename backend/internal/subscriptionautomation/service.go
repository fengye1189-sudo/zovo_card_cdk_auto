// Package subscriptionautomation is the isolated boundary for GPT
// subscription/recharge automation. It deliberately does not import the
// existing CDK or card-inventory handlers, so enabling its read-only
// endpoints cannot change the current fulfillment flow.
package subscriptionautomation

import (
	"context"
	"os"
	"strings"
	"time"
)

// Mode is intentionally read-only until the real provider contracts and
// idempotency/recovery tables are wired in.
type Mode string

const (
	ModeDisabled Mode = "disabled"
	ModePreview  Mode = "preview"
)

type Service struct {
	mode Mode
}

func NewFromEnv() *Service {
	// The default is disabled. Setting SUBSCRIPTION_AUTOMATION_MODE=preview
	// exposes only health/route previews; it never creates an upstream order.
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SUBSCRIPTION_AUTOMATION_MODE"))) {
	case "preview", "dry-run", "dry_run":
		return &Service{mode: ModePreview}
	default:
		return &Service{mode: ModeDisabled}
	}
}

func (s *Service) Mode() Mode {
	if s == nil {
		return ModeDisabled
	}
	return s.mode
}

type Status struct {
	Name      string `json:"name"`
	Mode      Mode   `json:"mode"`
	ReadOnly  bool   `json:"read_only"`
	Provider  string `json:"provider"`
	CheckedAt int64  `json:"checked_at"`
	Message   string `json:"message"`
}

func (s *Service) Status(_ context.Context) Status {
	mode := s.Mode()
	message := "second automation is disabled; existing flow is unchanged"
	if mode == ModePreview {
		message = "preview only; no upstream orders are created"
	}
	return Status{
		Name:      "subscription-automation",
		Mode:      mode,
		ReadOnly:  true,
		Provider:  "none",
		CheckedAt: time.Now().Unix(),
		Message:   message,
	}
}

type Preview struct {
	Product   string `json:"product"`
	Providers []string `json:"providers"`
	CanCreate bool `json:"can_create"`
	Message   string `json:"message"`
}

func (s *Service) Preview(product string) Preview {
	product = strings.TrimSpace(product)
	if product == "" {
		product = "plus"
	}
	return Preview{
		Product:   product,
		Providers: []string{"zovo", "orbitcard", "jz-rescue"},
		CanCreate: false,
		Message:   "preview only: provider adapters and order persistence are not enabled",
	}
}
