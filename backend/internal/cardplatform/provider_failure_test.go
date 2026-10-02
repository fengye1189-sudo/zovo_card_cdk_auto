package cardplatform

import (
	"testing"

	"github.com/tuzi/cdk-recharge-system/internal/provider"
)

func TestAPIErrorFailureClassification(t *testing.T) {
	cases := []struct {
		status int
		want   provider.FailureClass
	}{
		{429, provider.FailureRateLimited},
		{401, provider.FailureConfig},
		{502, provider.FailureTransient},
		{422, provider.FailureRejected},
	}
	for _, tc := range cases {
		got := (&APIError{HTTPStatus: tc.status}).FailureClass()
		if got != tc.want {
			t.Fatalf("status %d classified as %q, want %q", tc.status, got, tc.want)
		}
	}
}
