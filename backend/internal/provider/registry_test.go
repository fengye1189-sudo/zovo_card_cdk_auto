package provider

import (
	"context"
	"testing"
)

type testProvider struct{ name string }

func (p testProvider) Name() string { return p.name }
func (p testProvider) Capabilities() []Capability { return nil }
func (p testProvider) HealthCheck(_ context.Context) Health { return Health{Provider: p.name, Healthy: true} }
func (p testProvider) CreateOrder(_ context.Context, _ OrderRequest) (OrderResult, error) { return OrderResult{}, nil }
func (p testProvider) GetOrder(_ context.Context, _ string) (OrderResult, error) { return OrderResult{}, nil }

func TestRegistryNormalizesNamesAndRejectsDuplicates(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(testProvider{name: " ZOVO "}); err != nil { t.Fatal(err) }
	if _, ok := r.Get("zovo"); !ok { t.Fatal("registered provider not found") }
	if err := r.Register(testProvider{name: "zovo"}); err == nil { t.Fatal("duplicate provider was accepted") }
}
