package handler

import (
	"context"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/subscriptionautomation"
)

// AdminSubscriptionAutomationStatus is intentionally read-only. It is a
// separate surface for the future GPT subscription workflow and cannot touch
// existing CDK/card orders.
func AdminSubscriptionAutomationStatus(c *gin.Context) {
	service := subscriptionautomation.NewFromEnv()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"status": service.Status(ctx)})
}

// AdminSubscriptionAutomationPreview returns a dry-run routing preview only.
// It never creates a provider order, reserves a card, or changes an order.
func AdminSubscriptionAutomationPreview(c *gin.Context) {
	product := strings.TrimSpace(c.Query("product"))
	service := subscriptionautomation.NewFromEnv()
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"preview": service.Preview(product)})
}
