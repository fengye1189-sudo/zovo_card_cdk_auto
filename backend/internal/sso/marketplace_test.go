package sso

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

const testSecret = "a-32-byte-minimum-shared-secret-123456"

func signed(t *testing.T, p MarketplacePayload) string {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil { t.Fatal(err) }
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(testSecret)); _, _ = mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestVerifyMarketplaceToken(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	p := MarketplacePayload{V: 1, Sub: "shop-admin", Name: "Maple Admin", Role: "ADMIN", Iat: now.Unix(), Exp: now.Add(90*time.Second).Unix(), JTI: "unique-jti"}
	got, err := VerifyMarketplaceToken(signed(t, p), testSecret, now)
	if err != nil || got.Sub != p.Sub { t.Fatalf("got %#v, err %v", got, err) }
	if _, err := VerifyMarketplaceToken(signed(t, p)+"x", testSecret, now); err == nil { t.Fatal("tampered token accepted") }
}

func TestVerifyMarketplaceTokenRejectsNonAdminAndExpired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	base := MarketplacePayload{V: 1, Sub: "shop-admin", Role: "ADMIN", Iat: now.Unix(), Exp: now.Add(90*time.Second).Unix(), JTI: "unique-jti"}
	base.Role = "SELLER"
	if _, err := VerifyMarketplaceToken(signed(t, base), testSecret, now); err == nil { t.Fatal("seller token accepted") }
	base.Role, base.Exp = "ADMIN", now.Add(-time.Second).Unix()
	if _, err := VerifyMarketplaceToken(signed(t, base), testSecret, now); err == nil { t.Fatal("expired token accepted") }
}
