package provider

import (
	"context"
	"testing"
)

func TestClassifyFailure(t *testing.T) {
	deadline := ClassifyFailure(context.DeadlineExceeded)
	if deadline.Class != FailureTransient || !deadline.Retryable {
		t.Fatal("deadline was not classified as retryable transient failure")
	}
	cancelled := ClassifyFailure(context.Canceled)
	if cancelled.Class != FailureCancelled || cancelled.Retryable {
		t.Fatal("cancelled request was marked retryable")
	}
}
