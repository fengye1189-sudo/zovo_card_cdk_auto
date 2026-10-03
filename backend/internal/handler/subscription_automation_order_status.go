package handler

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/subscriptionautomation"
)

// AdminSubscriptionAutomationOrderStatus is read-only. It never triggers a
// retry or a provider call; the watcher remains the sole active poller.
func AdminSubscriptionAutomationOrderStatus(c *gin.Context) {
	orderID := strings.TrimSpace(c.Param("id"))
	if orderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order id is required"})
		return
	}
	snapshot, err := subscriptionautomation.NewStore(nil).GetOrderSnapshot(orderID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "subscription order not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not read subscription order"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"read_only": true, "order": snapshot})
}
