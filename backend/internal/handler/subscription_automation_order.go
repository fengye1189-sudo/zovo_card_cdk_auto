package handler

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/subscriptionautomation"
)

type subscriptionDryRunRequest struct {
	ClientOrderNo string `json:"client_order_no"`
	Product string `json:"product"`
	PaymentRegion string `json:"payment_region"`
}

// AdminSubscriptionAutomationOrderPreview validates and persists an isolated
// dry-run order. It never calls an upstream, reserves a card, or changes a
// legacy order. It is available only when the second automation is explicitly
// placed in preview mode.
func AdminSubscriptionAutomationOrderPreview(c *gin.Context) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("SUBSCRIPTION_AUTOMATION_MODE")), "preview") {
		c.JSON(http.StatusConflict, gin.H{"error": "subscription automation is not in preview mode"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	var req subscriptionDryRunRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON"})
		return
	}
	req.ClientOrderNo = strings.TrimSpace(req.ClientOrderNo)
	req.Product = strings.TrimSpace(req.Product)
	req.PaymentRegion = strings.ToUpper(strings.TrimSpace(req.PaymentRegion))
	if len(req.ClientOrderNo) < 8 || len(req.ClientOrderNo) > 128 || req.Product == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "client_order_no and product are required"})
		return
	}
	orderID := "sub-preview-" + time.Now().UTC().Format("20060102150405.000000000")
	if err := subscriptionautomation.NewStore(nil).CreateOrder(orderID, req.ClientOrderNo, req.Product); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "client_order_no already exists or order could not be recorded"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusAccepted, gin.H{
		"preview_only": true,
		"order_id": orderID,
		"client_order_no": req.ClientOrderNo,
		"product": req.Product,
		"payment_region": req.PaymentRegion,
		"provider": "not_selected",
		"upstream_called": false,
		"message": "订单已写入隔离订阅表，尚未调用任何上游",
	})
}
