package auth

import (
	"os"

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
		secret = "your-secret-key-change-in-production"
	}
	return secret
}
