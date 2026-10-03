package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/subscriptionautomation"
)

// AdminSubscriptionAutomationSimulation is a no-side-effect diagnostic
// endpoint. It exercises the exact transition state machine used by routing.
func AdminSubscriptionAutomationSimulation(c *gin.Context) {
	scenario := strings.TrimSpace(c.Query("scenario"))
	result, err := subscriptionautomation.Simulate(scenario)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"simulation_only": true, "result": result})
}
