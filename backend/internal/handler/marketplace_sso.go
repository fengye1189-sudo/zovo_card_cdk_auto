package handler

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/auth"
	"github.com/tuzi/cdk-recharge-system/internal/db"
	"github.com/tuzi/cdk-recharge-system/internal/sso"
)

// MarketplaceSSO consumes a short lived, single-use assertion from the
// MaplePass administrator portal and starts the ordinary local admin session.
// No marketplace session cookie, password, or user email is accepted here.
func MarketplaceSSO(c *gin.Context) {
	payload, err := sso.VerifyMarketplaceToken(c.Query("token"), strings.TrimSpace(os.Getenv("CDK_SSO_SHARED_SECRET")), time.Now())
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_sso_token"})
		return
	}
	if err := db.ConsumeMarketplaceSSONonce(payload.JTI, payload.Exp); err != nil {
		if errors.Is(err, db.ErrSSONonceUsed) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "sso_token_already_used"})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "sso_unavailable"})
		return
	}

	username := "maple-sso-" + shortSSOSubject(payload.Sub)
	if auth.EmailLoginRequired() && (payload.EmailVerifiedUntil <= time.Now().Unix() || payload.EmailVerifiedUntil > time.Now().Add(12*time.Hour+time.Minute).Unix()) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请先从商城完成邮箱验证码登录"})
		return
	}
	c.Set("admin_email_verified_until", payload.EmailVerifiedUntil)
	identity, err := auth.EnsureMarketplaceAdmin(payload.Sub, username, payload.Name)
	if err != nil {
		if errors.Is(err, auth.ErrAccessRevoked) {
			c.JSON(http.StatusForbidden, gin.H{"error": "商城账号在兑换站的访问已停用，请联系站点管理员"})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "商城登录暂时无法完成，请稍后从商城重新打开"})
		return
	}
	if _, err := issueAdminSession(c, identity.ID, identity.Username, identity.Name); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "sso_session_failed"})
		return
	}
	db.WriteAudit(username, "marketplace_sso_login", payload.JTI, c.ClientIP())
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	// The Vue admin shell keeps only display metadata in local storage.  Send
	// the browser through its SSO completion page so a first-time browser can
	// read the new HttpOnly session cookie and populate that metadata without
	// ever receiving a password or a reusable token in the marketplace.
	returnPath := safeSSOReturnPath(c.Query("next"))
	c.Redirect(http.StatusFound, "/ops/login?sso=1&redirect="+url.QueryEscape(returnPath))
}

func shortSSOSubject(sub string) string {
	sub = strings.TrimSpace(sub)
	if len(sub) > 24 {
		return sub[:24]
	}
	if sub == "" {
		return "admin"
	}
	return sub
}

func safeSSOReturnPath(path string) string {
	// The marketplace supplies only these fixed paths.  `entry=maplepass` is
	// deliberately whitelisted so a marketplace launch performs a fresh page
	// navigation after a frontend update; arbitrary query strings are never
	// reflected into a redirect.
	switch path {
	case "/ops/orders", "/ops/cdkeys", "/ops/integration", "/ops/webhooks", "/ops/automation":
		return path
	case "/ops/orders?entry=maplepass", "/ops/cdkeys?entry=maplepass", "/ops/integration?entry=maplepass", "/ops/webhooks?entry=maplepass", "/ops/automation?entry=maplepass":
		return path
	default:
		return "/ops/automation"
	}
}
