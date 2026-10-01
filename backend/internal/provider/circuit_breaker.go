package provider

import (
	"sync"
	"time"
)

type CircuitState string

const (
	CircuitClosed   CircuitState = "closed"
	CircuitOpen     CircuitState = "open"
	CircuitHalfOpen CircuitState = "half_open"
)

// CircuitBreaker is deliberately transport-agnostic. Callers wrap one
// provider operation and report its result; it does not retry or mutate data.
type CircuitBreaker struct {
	mu              sync.Mutex
	state           CircuitState
	failures        int
	threshold       int
	cooldown        time.Duration
	openedAt        time.Time
}

func NewCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	if threshold < 1 {
		threshold = 3
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &CircuitBreaker{state: CircuitClosed, threshold: threshold, cooldown: cooldown}
}

func (b *CircuitBreaker) Allow(now time.Time) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == CircuitOpen && now.Sub(b.openedAt) >= b.cooldown {
		b.state = CircuitHalfOpen
		return true
	}
	return b.state != CircuitOpen
}

func (b *CircuitBreaker) Success() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.state, b.failures, b.openedAt = CircuitClosed, 0, time.Time{}
	b.mu.Unlock()
}

func (b *CircuitBreaker) Failure(now time.Time) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.failures >= b.threshold {
		b.state = CircuitOpen
		b.openedAt = now
	}
}

func (b *CircuitBreaker) State() CircuitState {
	if b == nil {
		return CircuitClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
