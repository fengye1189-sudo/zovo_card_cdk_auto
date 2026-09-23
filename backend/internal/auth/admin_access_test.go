package auth

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func accessFixture(t *testing.T) {
	t.Helper()
	previous := db.DB
	conn, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "access.db")+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	db.DB = conn
	t.Cleanup(func() { conn.Close(); db.DB = previous })
	_, err = conn.Exec(`CREATE TABLE admin_users(id INTEGER PRIMARY KEY AUTOINCREMENT,username TEXT UNIQUE NOT NULL,password_hash TEXT NOT NULL,display_name TEXT,is_active INTEGER DEFAULT 1,created_at DATETIME DEFAULT CURRENT_TIMESTAMP,updated_at DATETIME DEFAULT CURRENT_TIMESTAMP);
	INSERT INTO admin_users(username,password_hash,display_name,is_active) VALUES('legacy_owner','!','Legacy Owner',1),('inactive_legacy','!','Inactive',0);`)
	if err != nil {
		t.Fatal(err)
	}
	if err = InitAdminAccess(); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyAdminMigrationPreservesLoginAndInactiveState(t *testing.T) {
	accessFixture(t)
	identity, err := ResolveAdminSession(&CustomClaims{UserID: 1, Username: "legacy_owner", IsAdmin: true})
	if err != nil || identity.Role != RoleOwner {
		t.Fatalf("legacy owner locked out: %v %+v", err, identity)
	}
	if _, err = ResolveAdminSession(&CustomClaims{UserID: 2, Username: "inactive_legacy", IsAdmin: true}); err == nil {
		t.Fatal("inactive legacy admin accepted")
	}
	if _, err = db.DB.Exec(`UPDATE admin_access SET role='viewer' WHERE admin_user_id=1`); err != nil {
		t.Fatal(err)
	}
	if err = InitAdminAccess(); err != nil {
		t.Fatal(err)
	}
	identity, err = AdminByID(1)
	if err != nil || identity.Role != RoleViewer {
		t.Fatal("migration promoted existing viewer", err, identity)
	}
}

func TestSessionUsesDatabaseRoleAndRevocationVersion(t *testing.T) {
	accessFixture(t)
	claims := &CustomClaims{UserID: 1, Username: "legacy_owner", IsAdmin: true}
	if _, err := db.DB.Exec(`UPDATE admin_access SET role='viewer',session_version=session_version+1 WHERE admin_user_id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveAdminSession(claims); err == nil {
		t.Fatal("old owner session survived demotion")
	}
	claims.SessionVersion = 1
	identity, err := ResolveAdminSession(claims)
	if err != nil || identity.Role != RoleViewer {
		t.Fatal("fresh session did not use database role", identity, err)
	}
	if _, err = db.DB.Exec(`UPDATE admin_users SET is_active=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err = ResolveAdminSession(claims); err == nil {
		t.Fatal("disabled user authenticated")
	}
	claims.Username = "somebody_else"
	if _, err = ResolveAdminSession(claims); err == nil {
		t.Fatal("identity mismatch accepted")
	}
}

func TestLegacyMarketplaceSessionPersistsAndCannotReactivate(t *testing.T) {
	accessFixture(t)
	claims := &CustomClaims{UserID: 0, Username: "maple-sso-team-owner", IsAdmin: true}
	identity, err := ResolveAdminSession(claims)
	if err != nil || identity.ID <= 0 || identity.Source != "marketplace" || identity.Role != RoleOwner {
		t.Fatal("legacy SSO bridge failed", identity, err)
	}
	linked, err := EnsureMarketplaceAdmin("full-original-subject", claims.Username, "Owner")
	if err != nil || linked.ID != identity.ID {
		t.Fatal("SSO login duplicated identity", linked, err)
	}
	if _, err = db.DB.Exec(`UPDATE admin_users SET is_active=0 WHERE id=?`, identity.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = ResolveAdminSession(claims); err == nil {
		t.Fatal("old SSO cookie bypassed deactivation")
	}
	if _, err = EnsureMarketplaceAdmin("full-original-subject", claims.Username, "Owner"); err == nil {
		t.Fatal("new SSO assertion reactivated disabled user")
	}
}

func TestMarketplaceDoesNotPromoteDemotedMembers(t *testing.T) {
	accessFixture(t)
	member, err := EnsureMarketplaceAdmin("full-subject", "maple-sso-member", "Member")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.DB.Exec(`UPDATE admin_access SET role='operator',session_version=1 WHERE admin_user_id=?`, member.ID); err != nil {
		t.Fatal(err)
	}
	member, err = EnsureMarketplaceAdmin("full-subject", "maple-sso-member", "Member")
	if err != nil || member.Role != RoleOperator || member.SessionVersion != 1 {
		t.Fatal("SSO login elevated a member", member, err)
	}
	if _, err = EnsureMarketplaceAdmin("different-subject", "maple-sso-member", "Other"); err == nil {
		t.Fatal("truncated subject collision accepted")
	}
}

func TestRolePermissionsLeastPrivilege(t *testing.T) {
	for _, permission := range []string{"team.manage", "system.manage", "catalog.write"} {
		if HasPermission(RoleOperator, permission) || HasPermission(RoleViewer, permission) {
			t.Fatal("non-owner has privileged access", permission)
		}
	}
	if HasPermission(RoleViewer, "cdks.issue") || HasPermission(RoleViewer, "support.write") {
		t.Fatal("viewer can write")
	}
	if !HasPermission(RoleOperator, "support.write") || !HasPermission(RoleOperator, "cdks.issue") {
		t.Fatal("operator cannot do permitted work")
	}
	if HasPermission("", "dashboard.read") {
		t.Fatal("unknown role admitted")
	}
}
