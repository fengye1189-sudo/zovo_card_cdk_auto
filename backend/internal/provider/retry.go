package provider

import "time"

// RetryPolicy keeps transient upstream failures from causing tight retry
// loops. The result is deterministic and safe to persist as next_attempt_at.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 5, BaseDelay: 2 * time.Second, MaxDelay: 5 * time.Minute}
}

func (p RetryPolicy) Delay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = 2 * time.Second
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = 5 * time.Minute
	}
	d := p.BaseDelay
	for i := 1; i < attempt && d < p.MaxDelay; i++ {
		d *= 2
		if d > p.MaxDelay || d <= 0 {
			return p.MaxDelay
		}
	}
	return d
}

func (p RetryPolicy) CanRetry(attempt int) bool {
	return attempt >= 0 && (p.MaxAttempts <= 0 || attempt < p.MaxAttempts)
}
