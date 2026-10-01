package provider

import (
	"testing"
	"time"
)

func TestCircuitBreakerOpensAndHalfOpens(t *testing.T) {
	now := time.Unix(100, 0)
	b := NewCircuitBreaker(2, time.Minute)
	b.Failure(now)
	if !b.Allow(now) { t.Fatal("breaker opened too early") }
	b.Failure(now)
	if b.Allow(now) { t.Fatal("open breaker allowed request") }
	if !b.Allow(now.Add(time.Minute)) { t.Fatal("breaker did not half-open after cooldown") }
	b.Success()
	if b.State() != CircuitClosed { t.Fatal("success did not close breaker") }
}
