package auth

import (
	"os"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

type CustomClaims struct {
	UserID         int64  `json:"user_id"`
	IsAdmin        bool   `json:"is_admin"`
	Username       string `json:"username"`
	SessionVersion int64  `json:"session_version,omitempty"`
	EmailVerifiedUntil int64 `json:"email_verified_until,omitempty"`
	jwt.RegisteredClaims
}

func EmailLoginRequired() bool { return os.Getenv("MAPLE_ADMIN_EMAIL_REQUIRED") == "1" }

func JWTSecret() string {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		// Release configuration is rejected before the HTTP server starts. Keep
		// this branch only for local tests/setup; never turn a missing production
		// secret into a valid signing key.
		if strings.EqualFold(os.Getenv("SERVER_MODE"), "release") {
			return ""
		}
		secret = "your-secret-key-change-in-production"
	}
	return secret
}
