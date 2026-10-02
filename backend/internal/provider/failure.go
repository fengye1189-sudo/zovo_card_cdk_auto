package provider

import (
	"context"
	"errors"
	"net"
)

type FailureClass string

const (
	FailureTransient    FailureClass = "transient"
	FailureRateLimited  FailureClass = "rate_limited"
	FailureRejected     FailureClass = "rejected"
	FailureConfig       FailureClass = "configuration"
	FailureUnknown      FailureClass = "unknown"
	FailureCancelled    FailureClass = "cancelled"
)

type Failure struct {
	Class      FailureClass
	Retryable  bool
	RetryAfter int
	Message    string
}

// ClassifiedError allows provider adapters to preserve supplier-specific
// status details without leaking their response payloads into routing code.
type ClassifiedError interface {
	error
	FailureClass() FailureClass
	FailureRetryAfter() int
}

func ClassifyFailure(err error) Failure {
	if err == nil {
		return Failure{}
	}
	if errors.Is(err, context.Canceled) {
		return Failure{Class: FailureCancelled, Message: err.Error()}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Failure{Class: FailureTransient, Retryable: true, Message: err.Error()}
	}
	var classified ClassifiedError
	if errors.As(err, &classified) {
		class := classified.FailureClass()
		return Failure{Class: class, Retryable: class == FailureTransient || class == FailureRateLimited, RetryAfter: classified.FailureRetryAfter(), Message: err.Error()}
	}
	var network net.Error
	if errors.As(err, &network) {
		return Failure{Class: FailureTransient, Retryable: true, Message: err.Error()}
	}
	return Failure{Class: FailureUnknown, Message: err.Error()}
}
