package provider

import "context"

// Quote is an optional provider capability. Amounts are minor units to avoid
// floating-point money comparisons in routing decisions.
type Quote struct {
	Provider   string
	Product    string
	Available  bool
	CostMinor  int64
	Currency   string
	Stock      int64
	ObservedAt int64
}

// QuoteProvider is optional; legacy adapters remain valid without it.
type QuoteProvider interface {
	Provider
	Quote(context.Context, string) (Quote, error)
}

// InventoryProvider is optional and used for dashboards and preflight checks.
type InventoryProvider interface {
	Provider
	Inventory(context.Context, string) (int64, error)
}
