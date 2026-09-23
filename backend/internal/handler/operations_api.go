package handler

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/auth"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func InitOperationsAPI() error {
	_, err := db.DB.Exec(`CREATE TABLE IF NOT EXISTS operations_api_tokens (
 id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE,
 prefix TEXT NOT NULL, scopes TEXT NOT NULL, creator TEXT NOT NULL, creator_version INTEGER NOT NULL,
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, revoked_at INTEGER NOT NULL DEFAULT 0,
 last_used_at INTEGER NOT NULL DEFAULT 0, window_at INTEGER NOT NULL DEFAULT 0, window_count INTEGER NOT NULL DEFAULT 0
 );`)
	return err
}

func AdminAPITokens(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	rows, err := db.DB.Query("SELECT id,name,prefix,scopes,creator,created_at,expires_at,revoked_at,last_used_at FROM operations_api_tokens ORDER BY id DESC")
	if err != nil {
		localError(c, 503, "接入密钥列表暂不可用")
		return
	}
	defer rows.Close()
	list := []gin.H{}
	for rows.Next() {
		var id, created, expires, revoked, used int64
		var name, prefix, scopes, creator string
		if err = rows.Scan(&id, &name, &prefix, &scopes, &creator, &created, &expires, &revoked, &used); err != nil {
			localError(c, 503, "读取接入密钥失败")
			return
		}
		var permissions []string
		_ = json.Unmarshal([]byte(scopes), &permissions)
		list = append(list, gin.H{"id": id, "name": name, "prefix": prefix, "scopes": permissions, "creator": creator, "created_at": created, "expires_at": expires, "revoked_at": revoked, "last_used_at": used})
	}
	if rows.Err() != nil {
		localError(c, 503, "读取接入密钥失败")
		return
	}
	c.JSON(200, gin.H{"list": list})
}

func AdminCreateAPIToken(c *gin.Context) {
	var req struct {
		Name   string   `json:"name"`
		Days   int      `json:"days"`
		Scopes []string `json:"scopes"`
	}
	if !localBody(c, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 100 || req.Days < 1 || req.Days > 365 || len(req.Scopes) < 1 || len(req.Scopes) > 2 {
		localError(c, 400, "请填写名称、1–365 天有效期，并选择只读权限")
		return
	}
	scopes := []string{}
	seen := map[string]bool{}
	for _, s := range req.Scopes {
		if s != "records:read" && s != "products:read" {
			localError(c, 400, "不支持此权限")
			return
		}
		if !seen[s] {
			scopes = append(scopes, s)
			seen[s] = true
		}
	}
	creator, err := auth.AdminByUsername(c.GetString("username"))
	if err != nil || !creator.Active || creator.Role != auth.RoleOwner {
		localError(c, 403, "仅老板可创建接入密钥")
		return
	}
	var active int
	if err = db.DB.QueryRow("SELECT COUNT(*) FROM operations_api_tokens WHERE revoked_at=0 AND expires_at>?", time.Now().Unix()).Scan(&active); err != nil {
		localError(c, 503, "暂不能创建接入密钥")
		return
	}
	if active >= 30 {
		localError(c, 409, "最多保留 30 个有效接入密钥，请先撤销不用的密钥")
		return
	}
	random := make([]byte, 32)
	if _, err = rand.Read(random); err != nil {
		localError(c, 503, "暂不能创建接入密钥")
		return
	}
	token := "maple_ro_" + hex.EncodeToString(random)
	sum := sha256.Sum256([]byte(token))
	raw, _ := json.Marshal(scopes)
	now := time.Now().Unix()
	result, err := db.DB.Exec("INSERT INTO operations_api_tokens(name,token_hash,prefix,scopes,creator,creator_version,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?)", req.Name, hex.EncodeToString(sum[:]), token[:17], string(raw), creator.Username, creator.SessionVersion, now, now+int64(req.Days)*86400)
	if err != nil {
		localError(c, 503, "接入密钥创建失败")
		return
	}
	id, _ := result.LastInsertId()
	db.WriteAudit(creator.Username, "integration_token_create", "id="+strconv.FormatInt(id, 10), c.ClientIP())
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, gin.H{"id": id, "token": token, "message": "仅显示一次，请保存在调用系统的密钥设置中"})
}

func AdminRevokeAPIToken(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		localError(c, 400, "编号不正确")
		return
	}
	result, err := db.DB.Exec("UPDATE operations_api_tokens SET revoked_at=? WHERE id=? AND revoked_at=0", time.Now().Unix(), id)
	if err != nil {
		localError(c, 503, "撤销失败")
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		localError(c, 404, "密钥不存在或已经撤销")
		return
	}
	db.WriteAudit(c.GetString("username"), "integration_token_revoke", "id="+strconv.FormatInt(id, 10), c.ClientIP())
	c.JSON(200, gin.H{"ok": true})
}

// Integration keys work only on explicitly mounted read-only endpoints.
func IntegrationReadAuth(scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if c.Request.Method != http.MethodGet {
			c.AbortWithStatusJSON(405, gin.H{"error": "此接入仅支持查询"})
			return
		}
		parts := strings.Fields(c.GetHeader("Authorization"))
		if len(parts) != 2 || parts[0] != "Bearer" || !strings.HasPrefix(parts[1], "maple_ro_") || len(parts[1]) != 73 {
			c.AbortWithStatusJSON(401, gin.H{"error": "接入密钥无效"})
			return
		}
		sum := sha256.Sum256([]byte(parts[1]))
		var id, version int64
		var raw, username string
		now := time.Now().Unix()
		err := db.DB.QueryRow("SELECT id,scopes,creator,creator_version FROM operations_api_tokens WHERE token_hash=? AND revoked_at=0 AND expires_at>?", hex.EncodeToString(sum[:]), now).Scan(&id, &raw, &username, &version)
		if err != nil {
			c.AbortWithStatusJSON(401, gin.H{"error": "接入密钥无效或已过期"})
			return
		}
		owner, err := auth.AdminByUsername(username)
		if err != nil || !owner.Active || owner.Role != auth.RoleOwner || owner.SessionVersion != version {
			c.AbortWithStatusJSON(401, gin.H{"error": "接入密钥已失效"})
			return
		}
		var scopes []string
		_ = json.Unmarshal([]byte(raw), &scopes)
		allowed := false
		for _, s := range scopes {
			if s == scope {
				allowed = true
			}
		}
		if !allowed {
			c.AbortWithStatusJSON(403, gin.H{"error": "密钥没有此查询权限"})
			return
		}
		window := now / 60
		result, err := db.DB.Exec("UPDATE operations_api_tokens SET last_used_at=?,window_count=CASE WHEN window_at=? THEN window_count+1 ELSE 1 END,window_at=? WHERE id=? AND revoked_at=0 AND expires_at>? AND (window_at<>? OR window_count<60)", now, window, window, id, now, window)
		if err != nil {
			c.AbortWithStatusJSON(503, gin.H{"error": "接入服务暂不可用"})
			return
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			c.Header("Retry-After", "60")
			c.AbortWithStatusJSON(429, gin.H{"error": "每分钟最多查询 60 次"})
			return
		}
		c.Set("username", "integration:"+strconv.FormatInt(id, 10))
		c.Next()
	}
}
