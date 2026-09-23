// Package sso verifies the short-lived one-time handoff assertions issued by
// maple1189ai.com. It intentionally uses a compact HMAC envelope instead of
// accepting marketplace cookies or passwords on this service.
package sso

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var ErrInvalidToken = errors.New("invalid marketplace sso token")

type MarketplacePayload struct {
	V    int    `json:"v"`
	Sub  string `json:"sub"`
	Name string `json:"name"`
	Role string `json:"role"`
	Iat  int64  `json:"iat"`
	Exp  int64  `json:"exp"`
	JTI  string `json:"jti"`
	EmailVerifiedUntil int64 `json:"email_verified_until,omitempty"`
}

func VerifyMarketplaceToken(token, secret string, now time.Time) (MarketplacePayload, error) {
	if len(secret) < 32 || len(token) > 4096 {
		return MarketplacePayload{}, ErrInvalidToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return MarketplacePayload{}, ErrInvalidToken
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(parts[0]))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(parts[1]) != len(expected) || subtle.ConstantTimeCompare([]byte(parts[1]), []byte(expected)) != 1 {
		return MarketplacePayload{}, ErrInvalidToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(raw) > 2048 {
		return MarketplacePayload{}, ErrInvalidToken
	}
	var p MarketplacePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return MarketplacePayload{}, ErrInvalidToken
	}
	nowUnix := now.Unix()
	if p.V != 1 || p.Role != "ADMIN" || strings.TrimSpace(p.Sub) == "" || strings.TrimSpace(p.JTI) == "" ||
		p.Iat <= 0 || p.Exp < nowUnix || p.Exp-p.Iat > 120 || p.Iat > nowUnix+30 {
		return MarketplacePayload{}, ErrInvalidToken
	}
	return p, nil
}
