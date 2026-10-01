package handler

import (
	"context"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/jzactivation"
	"github.com/tuzi/cdk-recharge-system/internal/provider"
)

// AdminProviderPreview is read-only: it checks upstream health and quotes but
// never creates an order, opens a card, or changes routing state.
func AdminProviderPreview(c *gin.Context) {
	product := strings.TrimSpace(c.Query("product"))
	if product == "" {
		product = "plus"
	}
	registry := provider.NewRegistry()
	_ = registry.Register(cardplatform.NewProviderAdapter(nil))
	_ = registry.Register(jzactivation.NewProviderAdapter(nil))
	router := provider.NewRouter(registry)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	candidates, err := router.PreviewQuotes(ctx, product, nil)
	if err != nil {
		localError(c, 503, "上游报价暂时不可用")
		return
	}
	items := make([]gin.H, 0, len(candidates))
	for _, item := range candidates {
		items = append(items, gin.H{
			"provider": item.Quote.Provider,
			"product": item.Quote.Product,
			"available": item.Quote.Available,
			"stock": item.Quote.Stock,
			"cost_minor": item.Quote.CostMinor,
			"currency": item.Quote.Currency,
			"observed_at": item.Quote.ObservedAt,
			"rank": item.Rank,
		})
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"product": product, "preview_only": true, "candidates": items})
}
