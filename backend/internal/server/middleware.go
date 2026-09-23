package server

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/tuzi/cdk-recharge-system/internal/auth"
)

// allowedOrigins 来自环境变量 CORS_ALLOWED_ORIGINS（逗号分隔），默认仅本地开发端口。
func allowedOrigins() []string {
	if raw := strings.TrimSpace(os.Getenv("CORS_ALLOWED_ORIGINS")); raw != "" {
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if v := strings.TrimSpace(p); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	return []string{
		"http://localhost:5173", "http://127.0.0.1:5173",
		"http://localhost:5174", "http://127.0.0.1:5174",
	}
}

// SecurityHeadersMiddleware 设置一组基础安全响应头；HSTS 仅在 HTTPS 下下发。
func SecurityHeadersMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")

		// 仅当确实走 HTTPS 时才下发 HSTS（直连 TLS 或反代标记 X-Forwarded-Proto=https）
		if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		c.Next()
	}
}

func CORSMiddleware() gin.HandlerFunc {
	allowed := allowedOrigins()
	allowAll := len(allowed) == 1 && allowed[0] == "*"

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")

		// 按白名单回显 Origin（不再用 "*" 搭配 credentials 的不安全组合）
		if allowAll {
			c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		} else if origin != "" {
			for _, o := range allowed {
				if o == origin {
					c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
					c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
					c.Writer.Header().Add("Vary", "Origin")
					break
				}
			}
		}

		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT, DELETE, PATCH")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

func isUnsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func JWTAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 优先 Authorization: Bearer（非浏览器/兼容旧前端）；否则读 HttpOnly cookie
		var tokenStr string
		fromCookie := false
		if authHeader := c.GetHeader("Authorization"); authHeader != "" {
			parts := strings.Split(authHeader, " ")
			if len(parts) != 2 || parts[0] != "Bearer" {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid authorization header"})
				c.Abort()
				return
			}
			tokenStr = parts[1]
		} else if ck, err := c.Cookie("auth_token"); err == nil && ck != "" {
			tokenStr = ck
			fromCookie = true
		} else {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Missing authorization"})
			c.Abort()
			return
		}

		claims := &auth.CustomClaims{}
		token, err := jwt.ParseWithClaims(tokenStr, claims, func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return []byte(auth.JWTSecret()), nil
		})
		if err != nil || !token.Valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired token"})
			c.Abort()
			return
		}
		identity, err := auth.ResolveAdminSession(claims)
		if err != nil {
			if !errors.Is(err, auth.ErrAccessRevoked) && !errors.Is(err, sql.ErrNoRows) {
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "登录状态暂时无法核对，请稍后刷新"})
				return
			}
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "登录已失效，请重新登录", "error_code": "unauthorized"})
			return
		}

		// 基于 cookie 认证的写操作必须通过 CSRF 双提交校验（Bearer 头不受 CSRF 影响，跳过）
		if fromCookie && isUnsafeMethod(c.Request.Method) {
			csrfHeader := c.GetHeader("X-CSRF-Token")
			csrfCookie, _ := c.Cookie("csrf_token")
			if csrfHeader == "" || csrfCookie == "" ||
				subtle.ConstantTimeCompare([]byte(csrfHeader), []byte(csrfCookie)) != 1 {
				c.JSON(http.StatusForbidden, gin.H{"error": "CSRF 校验失败"})
				c.Abort()
				return
			}
		}

		c.Set("user_id", identity.ID)
		c.Set("is_admin", true)
		c.Set("username", identity.Username)
		c.Set("admin_name", identity.Name)
		c.Set("admin_role", identity.Role)
		c.Set("admin_source", identity.Source)
		if claims.ExpiresAt != nil {
			c.Set("session_expires_at", claims.ExpiresAt.Time)
		}
		c.Next()
	}
}

func AdminAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		isAdmin, ok := c.Get("is_admin")
		if !ok || isAdmin != true || !adminRouteAllowed(c.GetString("admin_role"), c.Request.Method, c.FullPath()) {
			c.JSON(http.StatusForbidden, gin.H{"error": "当前账号没有此操作权限", "error_code": "permission_denied"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// AdminPermissionMiddleware applies explicit permissions to new endpoints.
// Use after JWTAuthMiddleware; AdminAuthMiddleware remains the default-deny route gate.
func AdminPermissionMiddleware(permission string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !auth.HasPermission(c.GetString("admin_role"), permission) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "当前账号没有此操作权限", "error_code": "permission_denied"})
			return
		}
		c.Next()
	}
}

func adminRouteAllowed(role, method, route string) bool {
	if role == auth.RoleOwner {
		return true
	}
	if role != auth.RoleOperator && role != auth.RoleViewer {
		return false
	}
	if method == http.MethodPost && route == "/api/v1/auth/admin/change-password" {
		return true
	}
	if (method == http.MethodPost && route == "/api/v1/admin/local-cdks/search") || (method == http.MethodGet && route == "/api/v1/admin/local-cdks/reserve") {
		return true
	}
	if method == http.MethodPost && route == "/api/v1/admin/operations/customers/search" {
		return true
	}
	if method == http.MethodGet {
		switch route {
		case "/api/v1/admin/local-cdks", "/api/v1/admin/operations/records", "/api/v1/admin/operations/records/export", "/api/v1/admin/operations/records/:id/support", "/api/v1/admin/operations/products", "/api/v1/admin/operations/dashboard", "/api/v1/admin/operations/reply-templates":
			return true
		}
	}
	if role == auth.RoleOperator {
		switch method + " " + route {
		case "POST /api/v1/admin/local-cdks", "POST /api/v1/admin/local-cdks/:id/disable", "POST /api/v1/admin/local-cdks/:id/reconcile", "POST /api/v1/admin/operations/records/:id/support", "PUT /api/v1/admin/operations/records/:id/support", "PATCH /api/v1/admin/operations/records/:id/support":
			return true
		}
	}
	return false
}
