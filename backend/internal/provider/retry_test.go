package provider

import (
	"testing"
	"time"
)

func TestRetryPolicyExponentialDelayAndLimit(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: 3 * time.Second}
	if p.Delay(1) != time.Second || p.Delay(2) != 2*time.Second || p.Delay(3) != 3*time.Second {
		t.Fatal("unexpected retry delays")
	}
	if !p.CanRetry(0) || !p.CanRetry(2) || p.CanRetry(3) {
		t.Fatal("unexpected retry limit")
	}
}
