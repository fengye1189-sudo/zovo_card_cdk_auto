package subscriptionautomation

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/db"
	"github.com/tuzi/cdk-recharge-system/internal/orbitcard"
	"github.com/tuzi/cdk-recharge-system/internal/provider"
)

// StartWatcher is opt-in. It is intentionally not started unless the
// deployment explicitly enables it, so adding this worker cannot change the
// existing fulfillment behavior.
func StartWatcher(ctx context.Context) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("SUBSCRIPTION_AUTOMATION_WATCHER_ENABLED")), "true") {
		return
	}
	store := NewStore(db.DB)
	providers := map[string]provider.Provider{"orbitcard": orbitcard.NewProviderAdapter(nil)}
	go runWatcher(ctx, store, providers)
}

func runWatcher(ctx context.Context, store *Store, providers map[string]provider.Provider) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done(): return
		case <-ticker.C:
			task, err := store.ClaimDueWatcher()
			if err != nil { log.Printf("[subscription-watcher] claim failed: %v", err); continue }
			if task == nil { continue }
			p := providers[task.Provider]
			if p == nil { _ = store.RescheduleWatcher(task.OrderID, "provider_not_registered", 30*time.Second, false); continue }
			result, err := p.GetOrder(ctx, task.ExternalID)
			if err != nil { _ = store.RescheduleWatcher(task.OrderID, err.Error(), retryDelay(task.Attempts), time.Now().After(task.Deadline)); continue }
			switch strings.ToLower(strings.TrimSpace(result.Status)) {
			case "succeeded":
				_ = store.FinishWatcher(task.OrderID, result.Status, result.ExternalID, "SUCCEEDED", "")
			case "failed", "declined", "rejected", "unsupported":
				_ = store.FinishWatcher(task.OrderID, result.Status, result.ExternalID, "TERMINAL_FAILURE", result.Status)
			default:
				_ = store.RescheduleWatcher(task.OrderID, "", retryDelay(task.Attempts), time.Now().After(task.Deadline))
			}
		}
	}
}

func retryDelay(attempts int) time.Duration {
	switch { case attempts <= 1: return 2*time.Second; case attempts == 2: return 5*time.Second; case attempts == 3: return 10*time.Second; default: return 30*time.Second }
}
