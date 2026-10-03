package subscriptionautomation

import (
	"context"
	"os"
	"testing"
)

func TestDefaultsToDisabled(t *testing.T) {
	os.Unsetenv("SUBSCRIPTION_AUTOMATION_MODE")
	s := NewFromEnv()
	if s.Mode() != ModeDisabled {
		t.Fatalf("mode=%q, want disabled", s.Mode())
	}
	if got := s.Status(context.Background()); !got.ReadOnly || got.Mode != ModeDisabled {
		t.Fatalf("unexpected status: %#v", got)
	}
}

func TestPreviewNeverCreatesOrders(t *testing.T) {
	os.Setenv("SUBSCRIPTION_AUTOMATION_MODE", "preview")
	t.Cleanup(func() { os.Unsetenv("SUBSCRIPTION_AUTOMATION_MODE") })
	s := NewFromEnv()
	got := s.Preview("")
	if got.Product != "plus" || got.CanCreate {
		t.Fatalf("unexpected preview: %#v", got)
	}
	if len(got.Providers) != 3 {
		t.Fatalf("providers=%v", got.Providers)
	}
}
