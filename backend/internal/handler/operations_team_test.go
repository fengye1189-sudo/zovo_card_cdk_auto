package handler

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/auth"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func teamFixture(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previous := db.DB
	conn, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "team.db")+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	db.DB = conn
	t.Cleanup(func() { conn.Close(); db.DB = previous })
	_, err = conn.Exec(`CREATE TABLE admin_users(id INTEGER PRIMARY KEY AUTOINCREMENT,username TEXT UNIQUE NOT NULL,password_hash TEXT NOT NULL,display_name TEXT,is_active INTEGER DEFAULT 1,created_at DATETIME DEFAULT CURRENT_TIMESTAMP,updated_at DATETIME DEFAULT CURRENT_TIMESTAMP);
	CREATE TABLE admin_audit_logs(id INTEGER PRIMARY KEY,username TEXT,action TEXT,detail TEXT,ip TEXT,created_at DATETIME DEFAULT CURRENT_TIMESTAMP);
	INSERT INTO admin_users(username,password_hash,display_name) VALUES('original_owner','!','Original Owner');`)
	if err != nil {
		t.Fatal(err)
	}
	if err = auth.InitAdminAccess(); err != nil {
		t.Fatal(err)
	}
}

func TestTeamCannotDisableOrDemoteLastOwner(t *testing.T) {
	teamFixture(t)
	if err := updateTeamMember(1, teamChange{}, true); !errors.Is(err, errLastOwner) {
		t.Fatalf("last-owner disable: %v", err)
	}
	if err := updateTeamMember(1, teamChange{Role: "viewer"}, false); !errors.Is(err, errLastOwner) {
		t.Fatalf("last-owner demotion: %v", err)
	}
	member, err := auth.AdminByID(1)
	if err != nil || !member.Active || member.Role != "owner" {
		t.Fatal("owner mutated after refused update", member, err)
	}
}

func TestConcurrentOwnerChangesKeepOneOwner(t *testing.T) {
	teamFixture(t)
	if _, err := db.DB.Exec(`INSERT INTO admin_users(username,password_hash) VALUES('second_owner','!');INSERT INTO admin_access(admin_user_id,role) VALUES(2,'owner')`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, id := range []int64{1, 2} {
		wg.Add(1)
		go func(memberID int64) {
			defer wg.Done()
			_ = updateTeamMember(memberID, teamChange{Role: "viewer"}, false)
		}(id)
	}
	wg.Wait()
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM admin_users u JOIN admin_access a ON a.admin_user_id=u.id WHERE u.is_active=1 AND a.role='owner'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("concurrent demotions left %d owners", count)
	}
}

func TestRoleAndPasswordChangesRevokeExistingSessions(t *testing.T) {
	teamFixture(t)
	if _, err := db.DB.Exec(`INSERT INTO admin_users(username,password_hash) VALUES('operator_user','!');INSERT INTO admin_access(admin_user_id,role) VALUES(2,'operator')`); err != nil {
		t.Fatal(err)
	}
	claims := &auth.CustomClaims{UserID: 2, Username: "operator_user", IsAdmin: true}
	if _, err := auth.ResolveAdminSession(claims); err != nil {
		t.Fatal(err)
	}
	if err := updateTeamMember(2, teamChange{Role: "viewer"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.ResolveAdminSession(claims); err == nil {
		t.Fatal("demotion did not revoke old login")
	}
	claims.SessionVersion = 1
	if err := changeAdminPasswordAndRevoke("operator_user", "Team-test-password-782!"); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.ResolveAdminSession(claims); err == nil {
		t.Fatal("password change did not revoke old login")
	}
}

func TestTeamCreateHashesPasswordAndDoesNotReturnIt(t *testing.T) {
	teamFixture(t)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("admin_role", "owner"); c.Set("username", "original_owner"); c.Next() })
	r.POST("/team", AdminTeamCreate)
	r.GET("/team", AdminTeamList)
	payload := `{"username":"reader_user","name":"Reader","password":"Team-test-password-981!","role":"viewer","is_active":true}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/team", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("create %d: %s", w.Code, w.Body.String())
	}
	var stored string
	if err := db.DB.QueryRow(`SELECT password_hash FROM admin_users WHERE username='reader_user'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "$2") {
		t.Fatal("password is not bcrypt hashed")
	}
	if ok, _ := db.VerifyAdminPassword(stored, "Team-test-password-981!"); !ok {
		t.Fatal("stored hash invalid")
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/team", nil))
	if strings.Contains(w.Body.String(), "password") || strings.Contains(w.Body.String(), "Team-test") || strings.Contains(w.Body.String(), stored) {
		// login_source=password is allowed; credential fields and values are not.
		var data map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		for _, entry := range data["members"].([]any) {
			member := entry.(map[string]any)
			if _, ok := member["password_hash"]; ok {
				t.Fatal("hash leaked")
			}
			if _, ok := member["password"]; ok {
				t.Fatal("password leaked")
			}
		}
		if strings.Contains(w.Body.String(), stored) || strings.Contains(w.Body.String(), "Team-test-password") {
			t.Fatal("credential leaked")
		}
	}
}

func TestTeamHandlerRejectsNonOwner(t *testing.T) {
	teamFixture(t)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("admin_role", "operator"); c.Next() })
	r.GET("/team", AdminTeamList)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/team", nil))
	if w.Code != 403 {
		t.Fatal("operator got team admin access", w.Code)
	}
}
