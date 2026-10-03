package handler

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/orbitcard"
	"github.com/tuzi/cdk-recharge-system/internal/provider"
	"github.com/tuzi/cdk-recharge-system/internal/subscriptionautomation"
)

type subscriptionRealOrderRequest struct {
	ClientOrderNo string          `json:"client_order_no"`
	Product       string          `json:"product"`
	PaymentRegion string          `json:"payment_region"`
	CardID        int64           `json:"card_id"`
	SessionJSON   json.RawMessage `json:"session_json"`
	Authorized    bool            `json:"authorized"`
	MaxServiceFee string          `json:"max_service_fee"`
}

// AdminSubscriptionAutomationCreateOrder is the guarded real-order entry
// point. It is separate from all legacy CDK and marketplace handlers.
func AdminSubscriptionAutomationCreateOrder(c *gin.Context) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("SUBSCRIPTION_AUTOMATION_REAL_ORDERS")), "true") || !strings.EqualFold(strings.TrimSpace(os.Getenv("ORBITCARD_ENABLED")), "true") {
		c.JSON(http.StatusConflict, gin.H{"error": "real subscription orders are disabled"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	var req subscriptionRealOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.SessionJSON) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request or missing session_json"})
		return
	}
	req.ClientOrderNo = strings.TrimSpace(req.ClientOrderNo)
	req.Product = strings.TrimSpace(req.Product)
	req.PaymentRegion = strings.ToUpper(strings.TrimSpace(req.PaymentRegion))
	if len(req.ClientOrderNo) < 8 || len(req.ClientOrderNo) > 128 || req.Product == "" || req.CardID <= 0 || !req.Authorized {
		c.JSON(http.StatusBadRequest, gin.H{"error": "client_order_no, product, card_id and authorized=true are required"})
		return
	}
	var session any
	if err := json.Unmarshal(req.SessionJSON, &session); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_json must be valid JSON"})
		return
	}
	store := subscriptionautomation.NewStore(nil)
	counts, err := store.PrimaryCounts(1000)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not read routing history"})
		return
	}
	health, err := store.ProviderHealth()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not read provider health"})
		return
	}
	decision, err := subscriptionautomation.SelectPrimaryWithHealth(counts, health)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no primary provider available"})
		return
	}
	// Until ZOVO subscription credentials are migrated into this isolated
	// service, real guarded orders are deliberately limited to OrbitCard.
	if decision.Provider != "orbitcard" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "orbitcard is not the selected primary provider"})
		return
	}
	orderID := "sub-" + time.Now().UTC().Format("20060102150405.000000000")
	if err := store.CreateOrder(orderID, req.ClientOrderNo, req.Product); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "client_order_no already exists or order could not be recorded"})
		return
	}
	metadata := map[string]string{"plan_type": req.Product, "payment_region": req.PaymentRegion, "card_id": strconv.FormatInt(req.CardID, 10), "session_json": string(req.SessionJSON), "max_service_fee": strings.TrimSpace(req.MaxServiceFee)}
	result, callErr := orbitcard.NewProviderAdapter(nil).CreateOrder(c.Request.Context(), provider.OrderRequest{Reference: req.ClientOrderNo, Product: req.Product, Metadata: metadata})
	if callErr != nil {
		_ = store.RecordAttempt(orderID, 1, "orbitcard", "PRIMARY", req.ClientOrderNo, req.ClientOrderNo, "", "UNKNOWN", "UNKNOWN", "create request failed", true, false)
		c.JSON(http.StatusAccepted, gin.H{"order_id": orderID, "status": "UNKNOWN", "manual_review": true})
		return
	}
	if err := store.RecordAttempt(orderID, 1, "orbitcard", "PRIMARY", req.ClientOrderNo, req.ClientOrderNo, result.ExternalID, result.Status, result.Status, "", false, orbitcard.IsTerminalFailure(result.Status)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not record provider attempt"})
		return
	}
	if strings.EqualFold(result.Status, "succeeded") {
		_ = store.MarkSuccess(orderID, "orbitcard")
		c.JSON(http.StatusOK, gin.H{"order_id": orderID, "status": "SUCCEEDED", "provider": "orbitcard"})
		return
	}
	if orbitcard.IsTerminalFailure(result.Status) {
		_ = store.MarkFailure(orderID, result.Status)
		c.JSON(http.StatusOK, gin.H{"order_id": orderID, "status": "FAILED", "provider": "orbitcard"})
		return
	}
	if result.ExternalID == "" {
		_ = store.MarkFailure(orderID, "missing external order id")
		c.JSON(http.StatusAccepted, gin.H{"order_id": orderID, "status": "MANUAL_REVIEW"})
		return
	}
	_ = store.ScheduleWatch(orderID, "orbitcard", result.ExternalID, time.Now().UTC().Add(15*time.Minute))
	c.JSON(http.StatusAccepted, gin.H{"order_id": orderID, "external_order_id": result.ExternalID, "status": result.Status, "provider": "orbitcard", "watching": true})
}
