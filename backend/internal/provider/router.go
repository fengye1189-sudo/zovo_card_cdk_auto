package provider

import (
	"context"
	"fmt"
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
