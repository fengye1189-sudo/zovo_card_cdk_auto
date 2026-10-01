package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
)

// PublicCDKGraceRecovery proxies the holder-scoped recovery flow. It accepts
// only redemption/preflight tokens returned by the public preflight step and
// never forwards the site's privileged Open API key to the browser-facing API.
func PublicCDKGraceRecovery(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 80<<10)
	var body struct {
		RedemptionToken string `json:"redemption_token"`
		PreflightToken  string `json:"preflight_token"`
		Confirmed       bool   `json:"confirmed"`
	}
	if c.ShouldBindJSON(&body) != nil || !body.Confirmed ||
		strings.TrimSpace(body.RedemptionToken) == "" || len(body.RedemptionToken) > 512 ||
		strings.TrimSpace(body.PreflightToken) == "" || len(body.PreflightToken) > 65536 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先检测账号并确认取消宽限期原订阅"})
		return
	}
	status, raw, err := cardplatform.NewFromSettings().RecoverSubscription(c.Request.Context(), body, deviceFrom(c))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "暂时无法确认处理结果，请重新检测账号，不要重复取消"})
		return
	}
	proxyPublicJSON(c, status, raw)
}
