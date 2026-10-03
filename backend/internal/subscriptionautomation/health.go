package subscriptionautomation

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
	"github.com/tuzi/cdk-recharge-system/internal/orbitcard"
	"github.com/tuzi/cdk-recharge-system/internal/provider"
)

// StartHealthChecker is opt-in and read-only. It updates only the isolated
// subscription_provider_health table.
func StartHealthChecker(ctx context.Context) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("SUBSCRIPTION_AUTOMATION_HEALTH_ENABLED")), "true") {
		return
	}
	store := NewStore(db.DB)
	providers := []provider.Provider{cardplatform.NewProviderAdapter(nil), orbitcard.NewProviderAdapter(nil)}
	go func() {
		check := func() {
			for _, p := range providers {
				checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				h := p.HealthCheck(checkCtx)
				cancel()
				state := HealthDegraded
				if h.Healthy {
					state = HealthHealthy
				}
				if err := store.UpsertProviderHealth(p.Name(), state, h.Message); err != nil {
					log.Printf("[subscription-health] %s: %v", p.Name(), err)
				}
			}
		}
		check()
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()
}
