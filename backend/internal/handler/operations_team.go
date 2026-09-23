package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/auth"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

var errLastOwner = errors.New("必须保留至少一位启用的老板账号")

func teamOwner(c *gin.Context) bool {
	if c.GetString("admin_role") != auth.RoleOwner {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "仅老板可以管理团队", "error_code": "permission_denied"})
		return false
	}
	return true
}

type teamMember struct {
	auth.AdminIdentity
	CreatedAt string `json:"created_at"`
}

func AdminTeamList(c *gin.Context) {
	if !teamOwner(c) {
		return
	}
	c.Header("Cache-Control", "no-store")
	rows, err := db.DB.Query(`SELECT u.id,u.username,COALESCE(NULLIF(u.display_name,''),u.username),u.is_active,a.role,a.login_source,COALESCE(CAST(u.created_at AS TEXT),'')
		FROM admin_users u JOIN admin_access a ON a.admin_user_id=u.id ORDER BY u.id`)
	if err != nil {
		localError(c, 503, "暂时无法读取团队")
		return
	}
	defer rows.Close()
	members := []teamMember{}
	for rows.Next() {
		var member teamMember
		var active int
		if err = rows.Scan(&member.ID, &member.Username, &member.Name, &active, &member.Role, &member.Source, &member.CreatedAt); err != nil {
			localError(c, 503, "暂时无法读取团队")
			return
		}
		member.Active = active == 1
		members = append(members, member)
	}
	if rows.Err() != nil {
		localError(c, 503, "暂时无法读取团队")
		return
	}
	c.JSON(200, gin.H{"members": members, "roles": []gin.H{{"value": "owner", "label": "老板", "description": "管理资金、规则、系统和团队"}, {"value": "operator", "label": "运营", "description": "查看记录、生成卡密、处理售后；不能修改资金和接入设置"}, {"value": "viewer", "label": "只读", "description": "查看运营数据和记录，不得修改"}}})
}

type teamChange struct {
	Username string `json:"username"`
	Name     string `json:"name"`
	Password string `json:"password"`
	Role     string `json:"role"`
	Active   *bool  `json:"is_active"`
}

func readTeamBody(c *gin.Context, body *teamChange) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(body); err != nil {
		localError(c, 400, "请求格式不正确")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		localError(c, 400, "请求格式不正确")
		return false
	}
	return true
}

func teamPasswordHash(username, password string) (string, error) {
	if password != strings.TrimSpace(password) || len(password) < 12 || len(password) > 72 || db.IsWeakPassword(password) || strings.EqualFold(username, password) {
		return "", errors.New("密码需为 12–72 字节，不含首尾空格，并避免常见弱密码")
	}
	return db.HashAdminPassword(password)
}

func AdminTeamCreate(c *gin.Context) {
	if !teamOwner(c) {
		return
	}
	var req teamChange
	if !readTeamBody(c, &req) {
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	req.Name = strings.TrimSpace(req.Name)
	if !validAdminUsername(req.Username) || strings.HasPrefix(req.Username, "maple-sso-") {
		localError(c, 400, "用户名需为 3–32 个字母、数字或下划线")
		return
	}
	if !auth.ValidRole(req.Role) || len(req.Name) > 120 {
		localError(c, 400, "姓名或角色不正确")
		return
	}
	hash, err := teamPasswordHash(req.Username, req.Password)
	if err != nil {
		localError(c, 400, err.Error())
		return
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	tx, err := db.DB.Begin()
	if err != nil {
		localError(c, 503, "暂时无法创建成员")
		return
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT INTO admin_users(username,password_hash,display_name,is_active) VALUES(?,?,?,?)`, req.Username, hash, req.Name, active)
	if err != nil {
		localError(c, 409, "用户名已存在，或暂时无法创建成员")
		return
	}
	id, err := result.LastInsertId()
	if err != nil {
		localError(c, 503, "暂时无法创建成员")
		return
	}
	if _, err = tx.Exec(`INSERT INTO admin_access(admin_user_id,role) VALUES(?,?)`, id, req.Role); err != nil {
		localError(c, 503, "暂时无法创建成员")
		return
	}
	if err = tx.Commit(); err != nil {
		localError(c, 503, "创建未完成，请刷新后核对")
		return
	}
	auditAdmin(c, "team_create", fmt.Sprintf("id=%d username=%s role=%s active=%t", id, req.Username, req.Role, active))
	c.JSON(201, gin.H{"id": id, "message": "成员已创建"})
}

func updateTeamMember(id int64, req teamChange, deactivate bool) error {
	tx, err := db.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// A write before reading serializes concurrent last-owner changes in SQLite.
	if _, err = tx.Exec(`UPDATE admin_access SET session_version=session_version WHERE admin_user_id=?`, id); err != nil {
		return err
	}
	var username, source, currentRole, currentName string
	var currentActive int
	if err = tx.QueryRow(`SELECT u.username,COALESCE(u.display_name,''),u.is_active,a.role,a.login_source FROM admin_users u JOIN admin_access a ON a.admin_user_id=u.id WHERE u.id=?`, id).Scan(&username, &currentName, &currentActive, &currentRole, &source); err != nil {
		return err
	}
	role := req.Role
	if role == "" {
		role = currentRole
	}
	if !auth.ValidRole(role) {
		return errors.New("角色不正确")
	}
	active := currentActive == 1
	if req.Active != nil {
		active = *req.Active
	}
	if deactivate {
		active = false
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = currentName
	}
	if len(name) > 120 {
		return errors.New("显示名称过长")
	}
	if currentRole == auth.RoleOwner && currentActive == 1 && (role != auth.RoleOwner || !active) {
		var owners int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM admin_users u JOIN admin_access a ON a.admin_user_id=u.id WHERE u.is_active=1 AND a.role='owner'`).Scan(&owners); err != nil {
			return err
		}
		if owners <= 1 {
			return errLastOwner
		}
	}
	if req.Username != "" && req.Username != username {
		return errors.New("不能更改登录用户名")
	}
	hash := ""
	if req.Password != "" {
		if source != "password" {
			return errors.New("商城成员的登录方式由商城管理")
		}
		hash, err = teamPasswordHash(username, req.Password)
		if err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`UPDATE admin_users SET display_name=?,is_active=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, name, active, id); err != nil {
		return err
	}
	if hash != "" {
		if _, err = tx.Exec(`UPDATE admin_users SET password_hash=? WHERE id=?`, hash, id); err != nil {
			return err
		}
	}
	revoke := 0
	if role != currentRole || active != (currentActive == 1) || hash != "" {
		revoke = 1
	}
	if _, err = tx.Exec(`UPDATE admin_access SET role=?,session_version=session_version+? WHERE admin_user_id=?`, role, revoke, id); err != nil {
		return err
	}
	return tx.Commit()
}

func teamUpdate(c *gin.Context, deactivate bool) {
	if !teamOwner(c) {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		localError(c, 400, "成员编号不正确")
		return
	}
	var req teamChange
	if !deactivate && !readTeamBody(c, &req) {
		return
	}
	if err = updateTeamMember(id, req, deactivate); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			localError(c, 404, "成员不存在")
			return
		}
		if errors.Is(err, errLastOwner) {
			localError(c, 409, err.Error())
			return
		}
		// Do not return database errors or password hashes.
		switch err.Error() {
		case "角色不正确", "显示名称过长", "不能更改登录用户名", "商城成员的登录方式由商城管理", "密码需为 12–72 字节，不含首尾空格，并避免常见弱密码":
			localError(c, 400, err.Error())
		default:
			localError(c, 503, "修改未完成，请刷新后重试")
		}
		return
	}
	action := "team_update"
	if deactivate {
		action = "team_deactivate"
	}
	auditAdmin(c, action, fmt.Sprintf("id=%d role=%s", id, req.Role))
	c.JSON(200, gin.H{"message": "已保存；权限、启用状态或密码变更会立即使旧登录失效"})
}

func AdminTeamUpdate(c *gin.Context) { teamUpdate(c, false) }

// Delete is a recoverable deactivation, preserving attribution in historical records.
func AdminTeamDelete(c *gin.Context) { teamUpdate(c, true) }

func changeAdminPasswordAndRevoke(username, password string) error {
	hash, err := teamPasswordHash(username, password)
	if err != nil {
		return err
	}
	tx, err := db.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE admin_users SET password_hash=?,updated_at=CURRENT_TIMESTAMP WHERE username=?`, hash, username); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE admin_access SET session_version=session_version+1 WHERE admin_user_id=(SELECT id FROM admin_users WHERE username=?)`, username); err != nil {
		return err
	}
	return tx.Commit()
}
