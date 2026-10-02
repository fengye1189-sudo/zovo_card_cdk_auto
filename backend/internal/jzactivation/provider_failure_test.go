package jzactivation

import (
	"testing"

	"github.com/tuzi/cdk-recharge-system/internal/provider"
)

func TestResponseErrorFailureClassification(t *testing.T) {
	err := (&ResponseError{HTTPStatus: 429, RetryAfter: 12})
	if err.FailureClass() != provider.FailureRateLimited || err.FailureRetryAfter() != 12 {
		t.Fatal("JZ rate limit classification lost retry-after")
	}
	disabled := (&ResponseError{Code: "channel_disabled"})
	if disabled.FailureClass() != provider.FailureConfig {
		t.Fatal("disabled JZ channel was not classified as configuration")
	}
}
