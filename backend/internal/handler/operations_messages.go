package handler

import (
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func defaultReplyTemplates() map[string]string {
	return map[string]string{
		"processing": "🍁 您的订单 {order_id} 正在处理中，请保留订单编号，稍后查询进度。",
		"completed":  "🍁 您的订单 {order_id} 已完成。感谢您的支持，如有问题请提供订单编号联系客服。",
		"review":     "🍁 您的订单 {order_id} 正在核对处理结果，请暂勿重复提交；我们核对后会告知您进展。",
		"failed":     "🍁 您的订单 {order_id} 未完成，我们会核对订单和资金情况后给您处理结果。",
	}
}
func AdminReplyTemplates(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	raw, e := db.GetSetting("operations_reply_templates")
	if e != nil {
		localError(c, 503, "回复模板暂时无法读取")
		return
	}
	templates := defaultReplyTemplates()
	if raw != "" {
		if json.Unmarshal([]byte(raw), &templates) != nil {
			localError(c, 503, "回复模板配置异常")
			return
		}
	}
	c.JSON(200, gin.H{"templates": templates, "revision": localHash(raw)})
}
func AdminSaveReplyTemplates(c *gin.Context) {
	var req struct {
		Templates map[string]string `json:"templates"`
		Revision  string            `json:"revision"`
	}
	if !localBody(c, &req) {
		return
	}
	if len(req.Templates) != 4 {
		localError(c, 400, "请填写四种订单状态的回复模板")
		return
	}
	for key := range defaultReplyTemplates() {
		v, ok := req.Templates[key]
		if !ok || len(v) > 3000 || strings.TrimSpace(v) == "" {
			localError(c, 400, "每条回复须为 1–3000 字节的文本")
			return
		}
	}
	tx, e := db.DB.Begin()
	if e != nil {
		localError(c, 503, "保存失败")
		return
	}
	defer tx.Rollback()
	if _, e = tx.Exec("INSERT OR IGNORE INTO site_settings(key,value) VALUES('operations_reply_templates','')"); e != nil {
		localError(c, 503, "保存失败")
		return
	}
	var raw string
	if e = tx.QueryRow("SELECT value FROM site_settings WHERE key='operations_reply_templates'").Scan(&raw); e != nil {
		localError(c, 503, "保存失败")
		return
	}
	if req.Revision != localHash(raw) {
		localError(c, 409, "模板已被其他管理员更新，请刷新后核对")
		return
	}
	next, _ := json.Marshal(req.Templates)
	if _, e = tx.Exec("UPDATE site_settings SET value=?,updated_at=CURRENT_TIMESTAMP WHERE key='operations_reply_templates'", string(next)); e != nil {
		localError(c, 503, "保存失败")
		return
	}
	if e = tx.Commit(); e != nil {
		localError(c, 503, "保存失败")
		return
	}
	db.WriteAudit(c.GetString("username"), "reply_templates_update", "four order templates updated", c.ClientIP())
	AdminReplyTemplates(c)
}
