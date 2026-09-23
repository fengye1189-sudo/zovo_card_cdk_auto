package handler

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/tuzi/cdk-recharge-system/internal/auth"
	"github.com/tuzi/cdk-recharge-system/internal/db"
	"github.com/tuzi/cdk-recharge-system/internal/sso"
)

func TestExistingOwnerPasswordLoginWithRoleSession(t *testing.T) {
	teamFixture(t)
	t.Setenv("JWT_SECRET", "test-only-admin-signing-secret-0123456789")
	hash, err := db.HashAdminPassword("Owner-test-login-873!")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.DB.Exec(`UPDATE admin_users SET password_hash=? WHERE id=1`, hash); err != nil {
		t.Fatal(err)
	}
	if _, err = db.DB.Exec(`UPDATE admin_access SET session_version=7 WHERE admin_user_id=1`); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.POST("/login", AdminLogin)
	req := httptest.NewRequest("POST", "/login", bytes.NewBufferString(`{"username":"original_owner","password":"Owner-test-login-873!"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal("existing owner login failed", w.Code)
	}
	var data struct {
		Token string `json:"token"`
		Role  string `json:"role"`
		Name  string `json:"name"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data.Role != "owner" || data.Name != "Original Owner" {
		t.Fatal("owner display or permissions lost")
	}
	claims := &auth.CustomClaims{}
	token, err := jwt.ParseWithClaims(data.Token, claims, func(token *jwt.Token) (any, error) { return []byte(auth.JWTSecret()), nil })
	if err != nil || !token.Valid || claims.UserID != 1 || claims.SessionVersion != 7 {
		t.Fatal("owner received invalid role-bound session", err)
	}
	if _, err = auth.ResolveAdminSession(claims); err != nil {
		t.Fatal("new owner session not usable", err)
	}
}

func TestMarketplaceHandlerPersistsIdentityAndHonorsDeactivation(t *testing.T) {
	teamFixture(t)
	t.Setenv("JWT_SECRET", "test-only-admin-signing-secret-0123456789")
	secret := "test-only-marketplace-shared-secret-0123456789"
	t.Setenv("CDK_SSO_SHARED_SECRET", secret)
	if _, err := db.DB.Exec(`CREATE TABLE marketplace_sso_nonces(jti TEXT PRIMARY KEY,expires_at INTEGER NOT NULL,used_at DATETIME DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.GET("/sso", MarketplaceSSO)
	assertion := func(jti string) string {
		now := time.Now().Unix()
		payload := sso.MarketplacePayload{V: 1, Sub: "existing-marketplace-admin", Name: "Maple Owner", Role: "ADMIN", Iat: now, Exp: now + 90, JTI: jti}
		raw, _ := json.Marshal(payload)
		encoded := base64.RawURLEncoding.EncodeToString(raw)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(encoded))
		return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	call := func(value string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/sso?token="+value+"&next=/ops/automation", nil))
		return w
	}
	first := assertion("first-sso")
	w := call(first)
	if w.Code != 302 {
		t.Fatal("marketplace SSO failed", w.Code)
	}
	var tokenValue string
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "auth_token" {
			tokenValue = cookie.Value
			if !cookie.HttpOnly {
				t.Fatal("session cookie not HttpOnly")
			}
		}
	}
	claims := &auth.CustomClaims{}
	token, err := jwt.ParseWithClaims(tokenValue, claims, func(token *jwt.Token) (any, error) { return []byte(auth.JWTSecret()), nil })
	if err != nil || !token.Valid || claims.UserID <= 0 {
		t.Fatal("SSO did not create persistent session", err)
	}
	identity, err := auth.ResolveAdminSession(claims)
	if err != nil || identity.Source != "marketplace" || identity.Role != "owner" {
		t.Fatal("SSO owner access failed", err)
	}
	if w = call(first); w.Code != 401 {
		t.Fatal("replayed SSO assertion accepted", w.Code)
	}
	if err = updateTeamMember(identity.ID, teamChange{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err = auth.ResolveAdminSession(claims); err == nil {
		t.Fatal("disabled SSO identity retained session")
	}
	if w = call(assertion("second-sso")); w.Code != 403 {
		t.Fatal("fresh SSO assertion bypassed deactivation", w.Code)
	}
}
