package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/auth"
	"github.com/tuzi/cdk-recharge-system/internal/config"
	"github.com/tuzi/cdk-recharge-system/internal/db"
	"github.com/tuzi/cdk-recharge-system/internal/handler"
	"github.com/tuzi/cdk-recharge-system/internal/plansync"
)

type Server struct {
	engine *gin.Engine
	cfg    *config.Config
}

func New(ctx context.Context, cfg *config.Config) (*Server, error) {
	// 生产模式下强制要求安全的 JWT secret，避免 token 被伪造
	if err := enforceJWTSecret(cfg.Server.Mode); err != nil {
		return nil, err
	}

	// Initialize database
	if err := db.Init(&cfg.Database); err != nil {
		return nil, err
	}
	if err := db.InitLocalCDK(); err != nil {
		return nil, err
	}
	if err := auth.InitAdminAccess(); err != nil {
		return nil, err
	}
	if err := handler.InitOperationsProducts(); err != nil {
		return nil, err
	}
	if err := handler.InitOperationsRecords(); err != nil {
		return nil, err
	}
	if err := handler.InitOperationsAPI(); err != nil {
		return nil, err
	}
	if err := handler.InitOperations(ctx); err != nil {
		return nil, err
	}
	if err := handler.InitCustomerExpiryNotifications(); err != nil {
		return nil, err
	}

	engine := gin.Default()

	// 不信任任意代理头（X-Forwarded-For 等），保证限流拿到的是真实来源 IP。
	// 若部署在已知反代后，用 TRUSTED_PROXIES（逗号分隔）显式声明。
	if tp := strings.TrimSpace(os.Getenv("TRUSTED_PROXIES")); tp != "" {
		_ = engine.SetTrustedProxies(splitAndTrim(tp))
	} else {
		_ = engine.SetTrustedProxies(nil)
	}

	// Setup middleware
	engine.Use(SecurityHeadersMiddleware())
	engine.Use(CORSMiddleware())

	// Setup routes
	setupRoutes(engine)

	// 启动卡台产品状态后台同步（每3分钟）
	plansync.Start(ctx)
	db.StartCDKQueryCleanup(ctx)
	handler.StartAutomation(ctx)
	handler.StartNotificationDispatcher(ctx)
	handler.StartCustomerExpiryNotifications(ctx)

	return &Server{
		engine: engine,
		cfg:    cfg,
	}, nil
}

// enforceJWTSecret 在 release 模式下拒绝使用空/默认密钥启动。
func enforceJWTSecret(mode string) error {
	secret := os.Getenv("JWT_SECRET")
	weak := secret == "" ||
		secret == "your-secret-key-change-in-production" ||
		secret == "dev-secret-key-change-in-production" ||
		len(secret) < 16
	if mode == "release" && weak {
		return fmt.Errorf("不安全的 JWT_SECRET：release 模式必须设置至少 16 位的随机 JWT_SECRET 环境变量")
	}
	if weak {
		log.Printf("⚠️  当前 JWT_SECRET 不安全（默认/过短）。仅可用于本地开发，部署前请设置强随机 JWT_SECRET（release 模式将拒绝启动）。")
	}
	return nil
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func setupRoutes(r *gin.Engine) {
	webDir := strings.TrimSpace(os.Getenv("WEB_DIR"))

	// Root route for quick manual checks in browser (仅当未托管前端时)
	if webDir == "" {
		r.GET("/", func(c *gin.Context) {
			c.JSON(200, gin.H{
				"status":   "ok",
				"message":  "Recharge System backend is running",
				"health":   "/health",
				"api_base": "/api/v1",
			})
		})
	}

	// Health check
	r.GET("/health", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		if db.DB == nil || db.DB.PingContext(ctx) != nil {
			c.JSON(503, gin.H{"status": "unavailable"})
			return
		}
		c.JSON(200, gin.H{
			"status":  "ok",
			"message": "Recharge System is running",
		})
	})

	// API v1 routes
	api := r.Group("/api/v1")
	{
		api.GET("", func(c *gin.Context) {
			c.JSON(200, gin.H{
				"status":  "ok",
				"message": "API v1 is available",
			})
		})

		auth := api.Group("/auth")
		{
			auth.POST("/admin/login", handler.AdminLogin)
			// 商城管理员无感进入：短期 HMAC 令牌，服务端验签并单次消费。
			auth.GET("/marketplace-sso", handler.MarketplaceSSO)
			auth.POST("/admin/logout", handler.AdminLogout)
			auth.GET("/admin/me", JWTAuthMiddleware(), handler.AdminMe)
			auth.POST("/admin/change-password", JWTAuthMiddleware(), AdminAuthMiddleware(), handler.AdminChangePassword)
		}

		// 首次安装向导（仅 pending；完成后 410）
		setup := api.Group("/setup")
		{
			setup.GET("/status", handler.SetupStatus)
			setup.POST("/bootstrap", handler.SetupBootstrap)
		}

		// 公开站点配置（品牌/皮肤，无密钥）
		api.GET("/public/site", handler.PublicSiteConfig)
		local := api.Group("/public/local-cdk", handler.LocalCDKLimit())
		local.POST("/preview", handler.LocalCDKPreview)
		local.POST("/replace", handler.LocalCDKReplace)
		local.POST("/preflight", handler.LocalCDKPreflight)
		local.POST("/redeem", handler.LocalCDKRedeem)
		local.POST("/result", handler.LocalCDKResult)

		// 卡台 CDK 公开兑换 BFF + 实时服务费展示
		// Existing batch redemption can legitimately issue many preview,
		// preflight, redeem and polling calls from one IP. Keep this group free
		// of the low-volume lookup limiter; individual handlers still enforce
		// POST bodies, size caps and no-store responses.
		pubCDK := api.Group("/public/cdk")
		{
			pubCDK.GET("/plans", handler.PublicCDKPlans)
			pubCDK.POST("/preview", handler.PublicCDKMutationLimit(), handler.PublicCDKPreview)
			pubCDK.POST("/preflight", handler.PublicCDKMutationLimit(), handler.PublicCDKPreflight)
			pubCDK.POST("/redeem", handler.PublicCDKMutationLimit(), handler.PublicCDKRedeem)
			pubCDK.POST("/result", handler.PublicCDKResult)
			// 刷新进度 / 任务查询：凭卡密反查本站绑定的 redemption_token
			pubCDK.POST("/result-by-code", handler.LocalCDKLimit(), handler.PublicCDKResultByCode)
			// 代理隐藏换码：密码 + 失败未扣款 CDK → 新码
			pubCDK.POST("/exchange", handler.PublicAgentCDKExchange)
		}

		// 卡密状态查询：是否已用 + 充值邮箱（不返回 token）
		lookup := api.Group("/lookup", handler.LocalCDKLimit())
		{
			lookup.POST("/cdk", handler.LookupCDKStatus)
			lookup.POST("/cdk/batch", handler.LookupCDKStatusBatch)
			// 兼容旧路径
			lookup.POST("/task", handler.LookupCDKStatus)
		}

		// 卡台 Webhook（须在卡台开发者页配置 https://你的域名/api/v1/webhooks/cardplatform）
		api.POST("/webhooks/cardplatform", handler.CardPlatformWebhook)

		// Marketplace-only, HMAC-authenticated order binding. This endpoint never
		// accepts a customer identity, plaintext CDK, credential, or browser cookie.
		internal := api.Group("/internal")
		internal.POST("/local-cdk/bind", handler.MarketplaceLocalCDKBind)
		// Marketplace-only issuer invoice lookup. The HMAC-authenticated caller
		// receives an upstream URL only after its order is bound to an
		// authoritative completion; no public endpoint exposes those URLs.
		internal.POST("/local-cdk/official-invoice", handler.MarketplaceOfficialInvoice)

		// 账单：粘贴 session 查 ChatGPT 订阅 + hosted_invoice（小助手同款）
		api.POST("/public/billing/check", handler.LocalCDKLimit(), handler.SessionBillingCheck)
		api.POST("/billing/check", handler.LocalCDKLimit(), handler.SessionBillingCheck)

		// Stats routes
		stats := api.Group("/stats")
		stats.Use(JWTAuthMiddleware(), AdminAuthMiddleware())
		{
			stats.GET("/system", handler.GetSystemStats)
		}

		// Admin routes
		admin := api.Group("/admin")
		admin.Use(JWTAuthMiddleware())
		admin.Use(AdminAuthMiddleware())
		{
			admin.GET("/operations/health", handler.AdminOperationsHealth)
			admin.GET("/operations/backups", handler.AdminOperationsBackups)
			admin.POST("/operations/backups", handler.AdminOperationsBackupCreate)
			admin.POST("/operations/backups/:name/verify", handler.AdminOperationsBackupVerify)
			admin.GET("/operations/backups/:name/download", handler.AdminOperationsBackupDownload)
			admin.GET("/operations/records", handler.OperationsRecordsList)
			admin.GET("/operations/records/export", handler.OperationsRecordsExport)
			admin.POST("/operations/customers/search", handler.OperationsCustomersSearch)
			admin.GET("/operations/records/:id/support", handler.OperationsSupportGet)
			admin.PUT("/operations/records/:id/support", handler.OperationsSupportSave)
			admin.GET("/operations/products", handler.OperationsProductsList)
			admin.POST("/operations/products", handler.OperationsProductsSave)
			admin.PUT("/operations/products/:id", handler.OperationsProductsSave)
			admin.GET("/operations/team", handler.AdminTeamList)
			admin.POST("/operations/team", handler.AdminTeamCreate)
			admin.PATCH("/operations/team/:id", handler.AdminTeamUpdate)
			admin.DELETE("/operations/team/:id", handler.AdminTeamDelete)
			admin.GET("/operations/api-tokens", handler.AdminAPITokens)
			admin.POST("/operations/api-tokens", handler.AdminCreateAPIToken)
			admin.DELETE("/operations/api-tokens/:id", handler.AdminRevokeAPIToken)
			admin.GET("/operations/reply-templates", handler.AdminReplyTemplates)
			admin.PUT("/operations/reply-templates", handler.AdminSaveReplyTemplates)
			admin.GET("/local-cdks", handler.LocalCDKList)
			admin.POST("/local-cdks/search", handler.LocalCDKSearch)
			admin.GET("/local-cdks/reserve", handler.LocalCodeReserveStatus)
			admin.GET("/direct-orders", handler.AdminDirectOrders)
			admin.GET("/automation/settings", handler.AdminAutomationSettings)
			admin.PUT("/automation/settings", handler.AdminAutomationSave)
			admin.POST("/automation/pause", handler.AdminAutomationPause)
			admin.GET("/automation/status", handler.AdminAutomationStatus)
			admin.GET("/automation/finance", handler.AdminFinance)
			admin.POST("/automation/finance/review", handler.AdminFinanceReview)
			admin.GET("/automation/notifications", handler.AdminNotificationSettings)
			admin.PUT("/automation/notifications", handler.AdminNotificationSave)
			admin.POST("/automation/notifications/test", handler.AdminNotificationTest)
			admin.GET("/direct-orders/:id", handler.AdminDirectDetail)
			admin.POST("/direct-orders/:id/:action", handler.AdminDirectAction)
			admin.GET("/local-card-choices", handler.LocalCardChoices)
			admin.POST("/local-cdks", handler.LocalCDKIssue)
			admin.POST("/local-cdks/:id/disable", handler.LocalCDKDisable)
			admin.GET("/local-cdk-settings", handler.LocalCDKGetSettings)
			admin.PUT("/local-cdk-settings", handler.LocalCDKPutSettings)
			admin.POST("/local-cdks/:id/reconcile", handler.LocalCDKReconcile)
			// 版本：本机 VERSION + GitHub 最新 release/tag
			admin.GET("/system/version", handler.AdminSystemVersion)
			admin.GET("/system/update/status", handler.AdminSystemUpdateStatus)
			admin.POST("/system/update", handler.AdminSystemUpdate)

			// 卡台 Open API：实时价格 / 余额 / 发码 / 列码 / CDK 订单
			admin.GET("/cardplatform/ping", handler.CardPlatformPing)
			admin.GET("/cardplatform/plans", handler.CardPlatformPlans)
			admin.GET("/cardplatform/balance", handler.CardPlatformBalance)
			admin.GET("/cardplatform/cdks", handler.CardPlatformListCDKs)
			admin.GET("/cardplatform/cdks/stored", handler.CardPlatformListStoredCDKs)
			admin.POST("/cardplatform/cdks", handler.CardPlatformIssueCDKs)
			admin.POST("/cardplatform/cdks/sync", handler.CardPlatformSyncUpstreamCDKs)
			admin.POST("/cardplatform/cdks/store", handler.CardPlatformStoreCDKCodes)
			admin.POST("/cardplatform/cdks/batch-disable", handler.CardPlatformBatchDisableCDKs)
			admin.POST("/cardplatform/cdks/batch-enable", handler.CardPlatformBatchEnableCDKs)
			admin.POST("/cardplatform/cdks/batch-note", handler.CardPlatformBatchSetCDKNote)
			admin.POST("/cardplatform/cdks/batch-clear-note", handler.CardPlatformBatchClearCDKNote)
			admin.PUT("/cardplatform/cdks/:id/note", handler.CardPlatformSetCDKNote)
			admin.POST("/cardplatform/cdks/:id/disable", handler.CardPlatformDisableCDK)
			admin.POST("/cardplatform/cdks/:id/enable", handler.CardPlatformEnableCDK)
			admin.GET("/cardplatform/cdk-orders", handler.CardPlatformListCDKOrders)
			admin.GET("/cardplatform/cdk-orders/:id", handler.CardPlatformGetCDKOrder)
			admin.DELETE("/cardplatform/cards/:id", handler.CardPlatformDeleteCard)

			// Webhook 事件列表 + 配置提示
			admin.GET("/webhooks/events", handler.AdminListWebhooks)

			// 本机出口 IP（卡台 API 白名单）
			admin.GET("/network/egress", handler.NetworkEgress)

			// Audit logs
			admin.GET("/audit-logs", handler.ListAuditLogs)

			// 站点设置（品牌/皮肤/卡台密钥保险箱）
			admin.GET("/settings", handler.AdminGetSettings)
			admin.PUT("/settings", handler.AdminPutSettings)

			// 自动选卡权重配置
			admin.GET("/card-selection/rules", handler.AdminGetCardSelectionRules)
			admin.PUT("/card-selection/rules", handler.AdminPutCardSelectionRules)
			admin.GET("/card-selection/plan-status", handler.AdminGetPlanStatus)
			admin.POST("/card-selection/sync", handler.AdminSyncPlanStatus)
			admin.GET("/card-selection/site-policy", handler.AdminGetSiteRedeemPolicy)
			admin.PUT("/card-selection/site-policy", handler.AdminPutSiteRedeemPolicy)

			// 卡健康：同卡失败 × 邮箱归因 → 坏卡冻结
			admin.GET("/card-health", handler.AdminListCardHealth)
			admin.GET("/card-health/policy", handler.AdminGetCardHealthPolicy)
			admin.PUT("/card-health/policy", handler.AdminPutCardHealthPolicy)
			admin.POST("/card-health/unblock", handler.AdminUnblockCard)
			admin.POST("/card-health/observe", handler.AdminReobserveCardOrder)
		}
		integration := api.Group("/integration")
		integration.GET("/records", handler.IntegrationReadAuth("records:read"), handler.OperationsRecordsList)
		integration.GET("/products", handler.IntegrationReadAuth("products:read"), handler.OperationsProductsList)
	}

	// 托管前端 SPA（当设置了 WEB_DIR 时）：真实存在的文件直出，其余回退到 index.html
	if webDir != "" {
		indexFile := filepath.Join(webDir, "index.html")
		r.NoRoute(func(c *gin.Context) {
			p := c.Request.URL.Path
			if strings.HasPrefix(p, "/api") || strings.HasPrefix(p, "/health") {
				c.JSON(404, gin.H{"error": "not found"})
				return
			}
			if c.Request.Method != "GET" && c.Request.Method != "HEAD" {
				c.JSON(404, gin.H{"error": "not found"})
				return
			}
			// 防目录穿越：path.Clean 后再拼接，确保不越出 webDir
			clean := path.Clean("/" + strings.TrimPrefix(p, "/"))
			if clean != "/" {
				fp := filepath.Join(webDir, filepath.FromSlash(clean))
				if st, err := os.Stat(fp); err == nil && !st.IsDir() {
					if strings.HasPrefix(clean, "/assets/") {
						c.Header("Cache-Control", "public, max-age=31536000, immutable")
					} else {
						c.Header("Cache-Control", "no-cache")
					}
					c.File(fp)
					return
				}
			}
			c.Header("Cache-Control", "no-store")
			c.File(indexFile)
		})
	}
}

func (s *Server) Run(addr string) error {
	return s.engine.Run(addr)
}
