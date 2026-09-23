package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/db"
)

const (
	RoleOwner    = "owner"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

var ErrAccessRevoked = errors.New("administrator access revoked")

// AdminIdentity contains display and authorization data, never login credentials.
type AdminIdentity struct {
	ID             int64  `json:"id"`
	Username       string `json:"username"`
	Name           string `json:"name"`
	Role           string `json:"role"`
	Active         bool   `json:"is_active"`
	Source         string `json:"login_source"`
	SessionVersion int64  `json:"-"`
}

func ValidRole(role string) bool {
	return role == RoleOwner || role == RoleOperator || role == RoleViewer
}

// InitAdminAccess preserves all existing administrators' owner permissions.
// New team members receive their explicit role in the same transaction as their account.
func InitAdminAccess() error {
	if db.DB == nil {
		return errors.New("database unavailable")
	}
	_, err := db.DB.Exec(`CREATE TABLE IF NOT EXISTS admin_access (
		admin_user_id INTEGER PRIMARY KEY,
		role TEXT NOT NULL CHECK(role IN ('owner','operator','viewer')),
		session_version INTEGER NOT NULL DEFAULT 0,
		login_source TEXT NOT NULL DEFAULT 'password',
		sso_subject TEXT NOT NULL DEFAULT '',
		FOREIGN KEY(admin_user_id) REFERENCES admin_users(id)
	);
	INSERT OR IGNORE INTO admin_access(admin_user_id,role)
	SELECT id,'owner' FROM admin_users;
	CREATE UNIQUE INDEX IF NOT EXISTS idx_admin_sso_subject ON admin_access(sso_subject) WHERE sso_subject<>'';`)
	return err
}

func AdminByID(id int64) (AdminIdentity, error) {
	return scanAdmin(db.DB.QueryRow(`SELECT u.id,u.username,COALESCE(NULLIF(u.display_name,''),u.username),u.is_active,a.role,a.session_version,a.login_source
		FROM admin_users u JOIN admin_access a ON a.admin_user_id=u.id WHERE u.id=?`, id))
}

func AdminByUsername(username string) (AdminIdentity, error) {
	return scanAdmin(db.DB.QueryRow(`SELECT u.id,u.username,COALESCE(NULLIF(u.display_name,''),u.username),u.is_active,a.role,a.session_version,a.login_source
		FROM admin_users u JOIN admin_access a ON a.admin_user_id=u.id WHERE u.username=?`, username))
}

type adminScanner interface{ Scan(...any) error }

func scanAdmin(row adminScanner) (AdminIdentity, error) {
	var identity AdminIdentity
	var active int
	err := row.Scan(&identity.ID, &identity.Username, &identity.Name, &active, &identity.Role, &identity.SessionVersion, &identity.Source)
	identity.Active = active == 1
	return identity, err
}

// ResolveAdminSession rechecks role, active status and session version on EVERY request.
// Old SSO cookies used id=0; bridge those signed identities once into persistent accounts.
func ResolveAdminSession(claims *CustomClaims) (AdminIdentity, error) {
	if EmailLoginRequired() && claims.EmailVerifiedUntil <= time.Now().Unix() { return AdminIdentity{}, ErrAccessRevoked }
	if !claims.IsAdmin || strings.TrimSpace(claims.Username) == "" {
		return AdminIdentity{}, ErrAccessRevoked
	}
	var identity AdminIdentity
	var err error
	if claims.UserID == 0 && strings.HasPrefix(claims.Username, "maple-sso-") && claims.SessionVersion == 0 {
		identity, err = AdminByUsername(claims.Username)
		if errors.Is(err, sql.ErrNoRows) {
			identity, err = EnsureMarketplaceAdmin("", claims.Username, claims.Username)
		}
		if err == nil && identity.Source != "marketplace" {
			return AdminIdentity{}, ErrAccessRevoked
		}
	} else {
		identity, err = AdminByID(claims.UserID)
	}
	if err != nil {
		return AdminIdentity{}, err
	}
	if !identity.Active || identity.Username != claims.Username || identity.SessionVersion != claims.SessionVersion || !ValidRole(identity.Role) {
		return AdminIdentity{}, ErrAccessRevoked
	}
	return identity, nil
}

// EnsureMarketplaceAdmin never reactivates, promotes, or changes an existing account.
// New accounts are created only after a verified marketplace administrator assertion.
func EnsureMarketplaceAdmin(subject, username, name string) (AdminIdentity, error) {
	if !strings.HasPrefix(username, "maple-sso-") {
		return AdminIdentity{}, ErrAccessRevoked
	}
	tx, err := db.DB.Begin()
	if err != nil {
		return AdminIdentity{}, err
	}
	defer tx.Rollback()
	// Acquire SQLite's writer lock before reading identity and role state.
	if _, err = tx.Exec(`UPDATE admin_access SET session_version=session_version WHERE admin_user_id=(SELECT MIN(admin_user_id) FROM admin_access)`); err != nil {
		return AdminIdentity{}, err
	}
	var id int64
	if subject != "" {
		err = tx.QueryRow(`SELECT admin_user_id FROM admin_access WHERE sso_subject=?`, subject).Scan(&id)
		if err != nil && err != sql.ErrNoRows {
			return AdminIdentity{}, err
		}
	}
	if id == 0 {
		err = tx.QueryRow(`SELECT id FROM admin_users WHERE username=?`, username).Scan(&id)
		if err == sql.ErrNoRows {
			result, e := tx.Exec(`INSERT INTO admin_users(username,password_hash,display_name,is_active) VALUES(?,'!',?,1)`, username, name)
			if e != nil {
				return AdminIdentity{}, e
			}
			id, e = result.LastInsertId()
			if e != nil {
				return AdminIdentity{}, e
			}
			_, err = tx.Exec(`INSERT INTO admin_access(admin_user_id,role,login_source,sso_subject) VALUES(?,'owner','marketplace',?)`, id, subject)
			if err != nil {
				return AdminIdentity{}, err
			}
		} else if err != nil {
			return AdminIdentity{}, err
		}
	}
	identity, err := scanAdmin(tx.QueryRow(`SELECT u.id,u.username,COALESCE(NULLIF(u.display_name,''),u.username),u.is_active,a.role,a.session_version,a.login_source FROM admin_users u JOIN admin_access a ON a.admin_user_id=u.id WHERE u.id=?`, id))
	if err != nil {
		return AdminIdentity{}, err
	}
	if identity.Source != "marketplace" || !identity.Active {
		return AdminIdentity{}, ErrAccessRevoked
	}
	if subject != "" {
		var existing string
		if err = tx.QueryRow(`SELECT sso_subject FROM admin_access WHERE admin_user_id=?`, id).Scan(&existing); err != nil {
			return AdminIdentity{}, err
		}
		if existing != "" && existing != subject {
			return AdminIdentity{}, fmt.Errorf("marketplace identity collision")
		}
		if _, err = tx.Exec(`UPDATE admin_access SET sso_subject=? WHERE admin_user_id=?`, subject, id); err != nil {
			return AdminIdentity{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return AdminIdentity{}, err
	}
	return identity, nil
}

// Permissions is also returned to the frontend so restricted users see useful navigation.
func Permissions(role string) []string {
	switch role {
	case RoleOwner:
		return []string{"records.read", "records.write", "cdks.issue", "support.write", "dashboard.read", "catalog.read", "catalog.write", "team.manage", "system.manage"}
	case RoleOperator:
		return []string{"records.read", "records.write", "cdks.issue", "support.write", "dashboard.read", "catalog.read"}
	case RoleViewer:
		return []string{"records.read", "dashboard.read", "catalog.read"}
	default:
		return []string{}
	}
}

func HasPermission(role, permission string) bool {
	for _, p := range Permissions(role) {
		if p == permission {
			return true
		}
	}
	return false
}
