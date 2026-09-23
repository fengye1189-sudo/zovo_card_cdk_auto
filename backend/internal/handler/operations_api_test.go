package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

type operationsAPIFixture struct {
	router    *gin.Engine
	token     string
	id        int64
	readCalls atomic.Int64
}

func newOperationsAPIFixture(t *testing.T) *operationsAPIFixture {
	t.Helper()
	teamFixture(t)
	if err := InitOperationsAPI(); err != nil {
		t.Fatal(err)
	}
	f := &operationsAPIFixture{router: gin.New()}
	admin := f.router.Group("/tokens", func(c *gin.Context) { c.Set("username", "original_owner"); c.Set("admin_role", "owner"); c.Next() })
	admin.POST("", AdminCreateAPIToken)
	admin.GET("", AdminAPITokens)
	admin.DELETE("/:id", AdminRevokeAPIToken)
	for _, route := range []struct{ path, scope string }{{"/records", "records:read"}, {"/products", "products:read"}} {
		f.router.GET(route.path, IntegrationReadAuth(route.scope), func(c *gin.Context) { f.readCalls.Add(1); c.JSON(200, gin.H{"list": []any{}}) })
		f.router.POST(route.path, IntegrationReadAuth(route.scope), func(c *gin.Context) { f.readCalls.Add(10000); c.JSON(200, gin.H{"unexpected_write": true}) })
	}
	w := f.call("POST", "/tokens", `{"name":"Test integration","days":3,"scopes":["records:read"]}`, false)
	if w.Code != 201 {
		t.Fatalf("create token %d: %s", w.Code, w.Body.String())
	}
	var data struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	f.id = data.ID
	f.token = data.Token
	return f
}

func (f *operationsAPIFixture) call(method, path, body string, bearer bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if bearer {
		r.Header.Set("Authorization", "Bearer "+f.token)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, r)
	return w
}

func TestOperationsAPITokenHashOnlyListAndScope(t *testing.T) {
	f := newOperationsAPIFixture(t)
	var hash string
	if err := db.DB.QueryRow(`SELECT token_hash FROM operations_api_tokens WHERE id=?`, f.id).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(f.token))
	if hash != hex.EncodeToString(sum[:]) || hash == f.token {
		t.Fatal("token not stored as hash")
	}
	w := f.call("GET", "/tokens", "", false)
	if w.Code != 200 || strings.Contains(w.Body.String(), f.token) || strings.Contains(w.Body.String(), hash) || strings.Contains(w.Body.String(), "token_hash") {
		t.Fatal("token list leaked credentials", w.Code)
	}
	if w = f.call("GET", "/records", "", true); w.Code != 200 {
		t.Fatal("allowed scope rejected", w.Code, w.Body.String())
	}
	if w = f.call("GET", "/products", "", true); w.Code != 403 {
		t.Fatal("wrong scope accepted", w.Code)
	}
	if w = f.call("GET", "/records", "", false); w.Code != 401 {
		t.Fatal("missing token accepted", w.Code)
	}
	if f.readCalls.Load() != 1 {
		t.Fatal("denied requests reached data handler")
	}
}

func TestOperationsAPITokenExpiryAndRevocation(t *testing.T) {
	for _, mode := range []string{"expired", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			f := newOperationsAPIFixture(t)
			if mode == "expired" {
				if _, err := db.DB.Exec(`UPDATE operations_api_tokens SET expires_at=? WHERE id=?`, time.Now().Add(-time.Second).Unix(), f.id); err != nil {
					t.Fatal(err)
				}
			} else {
				w := f.call("DELETE", fmt.Sprintf("/tokens/%d", f.id), "", false)
				if w.Code != 200 {
					t.Fatal("revoke failed", w.Code, w.Body.String())
				}
			}
			w := f.call("GET", "/records", "", true)
			if w.Code != 401 {
				t.Fatal("expired/revoked token accepted", w.Code)
			}
			if f.readCalls.Load() != 0 {
				t.Fatal("invalid token reached data handler")
			}
		})
	}
}

func TestOperationsAPITokenTracksOwnerAccess(t *testing.T) {
	for _, mode := range []string{"inactive", "demoted", "version_changed"} {
		t.Run(mode, func(t *testing.T) {
			f := newOperationsAPIFixture(t)
			var err error
			switch mode {
			case "inactive":
				_, err = db.DB.Exec(`UPDATE admin_users SET is_active=0 WHERE username='original_owner'`)
			case "demoted":
				_, err = db.DB.Exec(`UPDATE admin_access SET role='viewer' WHERE admin_user_id=1`)
			case "version_changed":
				_, err = db.DB.Exec(`UPDATE admin_access SET session_version=session_version+1 WHERE admin_user_id=1`)
			}
			if err != nil {
				t.Fatal(err)
			}
			w := f.call("GET", "/records", "", true)
			if w.Code != 401 || f.readCalls.Load() != 0 {
				t.Fatalf("%s owner's token accepted: %d", mode, w.Code)
			}
		})
	}
}

func TestOperationsAPIReadOnlyAndRateLimit(t *testing.T) {
	f := newOperationsAPIFixture(t)
	if w := f.call("POST", "/records", `{"action":"pay"}`, true); w.Code != 405 {
		t.Fatal("write method accepted", w.Code)
	}
	if f.readCalls.Load() != 0 {
		t.Fatal("write reached handler")
	}
	if w := f.call("GET", "/records", "", true); w.Code != 200 {
		t.Fatal("initial read failed", w.Code)
	}
	window := time.Now().Unix() / 60
	if _, err := db.DB.Exec(`UPDATE operations_api_tokens SET window_at=?,window_count=60 WHERE id=?`, window, f.id); err != nil {
		t.Fatal(err)
	}
	w := f.call("GET", "/records", "", true)
	if time.Now().Unix()/60 != window {
		t.Skip("request crossed minute boundary")
	}
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("rate cap did not block request", w.Code)
	}
	if f.readCalls.Load() != 1 {
		t.Fatal("blocked request reached read handler")
	}
}

func TestOperationsAPICannotCreateWriteScopeOrForNonOwner(t *testing.T) {
	f := newOperationsAPIFixture(t)
	for _, body := range []string{`{"name":"bad","days":1,"scopes":["payments:write"]}`, `{"name":"bad","days":0,"scopes":["records:read"]}`, `{"name":"bad","days":366,"scopes":["records:read"]}`} {
		if w := f.call("POST", "/tokens", body, false); w.Code != 400 {
			t.Fatal("invalid token specification accepted", w.Code)
		}
	}
	if _, err := db.DB.Exec(`UPDATE admin_access SET role='operator' WHERE admin_user_id=1`); err != nil {
		t.Fatal(err)
	}
	w := f.call("POST", "/tokens", `{"name":"unauthorized","days":1,"scopes":["records:read"]}`, false)
	if w.Code != 403 {
		t.Fatal("non-owner created integration token", w.Code)
	}
}
