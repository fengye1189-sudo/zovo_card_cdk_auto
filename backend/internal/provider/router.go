package provider

import (
	"context"
	"fmt"
	"sort"
)

// RouteRequest describes a provider selection without exposing business
// handlers to concrete suppliers.
type RouteRequest struct {
	Capability Capability
	Preferred  []string
}

// Router performs deterministic selection. Preferred names are evaluated in
// order; health is always checked before selection. It does not retry or
// mutate orders, so it is safe to introduce before wiring it into production.
type Router struct{ registry *Registry }

func NewRouter(registry *Registry) *Router { return &Router{registry: registry} }

func (r *Router) Select(ctx context.Context, req RouteRequest) (Provider, Health, error) {
	if r == nil || r.registry == nil {
		return nil, Health{}, fmt.Errorf("provider registry is not configured")
	}
	names := req.Preferred
	if len(names) == 0 {
		names = r.registry.Names()
	}
	for _, name := range names {
		p, ok := r.registry.Get(name)
		if !ok || !supports(p, req.Capability) {
			continue
		}
		h := p.HealthCheck(ctx)
		if h.Healthy {
			return p, h, nil
		}
	}
	return nil, Health{}, fmt.Errorf("no healthy provider supports %q", req.Capability)
}

// QuoteCandidate is a read-only routing preview. It is intentionally
// separate from Select so callers can show the decision before enabling
// automatic order routing.
type QuoteCandidate struct {
	Provider Provider
	Health   Health
	Quote    Quote
	Rank     int
}

func (r *Router) PreviewQuotes(ctx context.Context, product string, preferred []string) ([]QuoteCandidate, error) {
	if r == nil || r.registry == nil {
		return nil, fmt.Errorf("provider registry is not configured")
	}
	names := preferred
	if len(names) == 0 {
		names = r.registry.Names()
	}
	out := make([]QuoteCandidate, 0, len(names))
	for rank, name := range names {
		p, ok := r.registry.Get(name)
		if !ok {
			continue
		}
		qp, ok := p.(QuoteProvider)
		if !ok {
			continue
		}
		h := p.HealthCheck(ctx)
		if !h.Healthy {
			continue
		}
		q, err := qp.Quote(ctx, product)
		if err != nil || !q.Available {
			continue
		}
		out = append(out, QuoteCandidate{Provider: p, Health: h, Quote: q, Rank: rank})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Quote.CostMinor != out[j].Quote.CostMinor {
			return out[i].Quote.CostMinor < out[j].Quote.CostMinor
		}
		return out[i].Rank < out[j].Rank
	})
	return out, nil
}

func supports(p Provider, capability Capability) bool {
	if capability == "" {
		return true
	}
	for _, item := range p.Capabilities() {
		if item == capability {
			return true
		}
	}
	return false
}
