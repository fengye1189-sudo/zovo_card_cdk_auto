package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"net/http"
	"strings"
)

// Uses the holder's redemption/preflight tokens, never the site's privileged API key.
func PublicCDKGraceRecovery(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 80<<10)
	var body struct {
		RedemptionToken string `json:"redemption_token"`
		PreflightToken  string `json:"preflight_token"`
		Confirmed       bool   `json:"confirmed"`
	}
	if c.ShouldBindJSON(&body) != nil || !body.Confirmed || strings.TrimSpace(body.RedemptionToken) == "" ||
		len(body.RedemptionToken) > 512 || strings.TrimSpace(body.PreflightToken) == "" || len(body.PreflightToken) > 65536 {
		c.JSON(400, gin.H{"error": "请先检测账号并确认取消宽限期原订阅"})
		return
	}
	status, raw, err := cardplatform.NewFromSettings().RecoverSubscription(c.Request.Context(), body, deviceFrom(c))
	if err != nil {
		c.JSON(502, gin.H{"error": "暂时无法确认处理结果，请重新检测账号，不要重复取消"})
		return
	}
	proxyPublicJSON(c, status, raw)
}
