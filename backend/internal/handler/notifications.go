package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

type notificationConfig struct {
	Enabled   bool   `json:"enabled"`
	Channel   string `json:"channel"`
	Recipient string `json:"recipient"`
	From      string `json:"from"`
	Secret    string `json:"secret,omitempty"`
}

func readNotificationConfig() (notificationConfig, int64, error) {
	var p notificationConfig
	var raw string
	var v int64
	e := db.DB.QueryRow("SELECT value,version FROM notification_config WHERE id=1").Scan(&raw, &v)
	if e == nil {
		e = json.Unmarshal([]byte(raw), &p)
	}
	return p, v, e
}

var telegramToken = regexp.MustCompile(`^[0-9]{5,20}:[A-Za-z0-9_-]{20,150}$`)
var telegramChat = regexp.MustCompile(`^-?[0-9]{1,20}$`)

func validNotification(p notificationConfig) bool {
	if len(p.Secret) > 200 || len(p.Recipient) > 254 || len(p.From) > 254 {
		return false
	}
	if p.Channel != "" && p.Channel != "email" && p.Channel != "telegram" {
		return false
	}
	if !p.Enabled {
		return true
	}
	if p.Channel == "telegram" {
		return telegramToken.MatchString(p.Secret) && telegramChat.MatchString(p.Recipient)
	}
	if p.Channel == "email" {
		to, e := mail.ParseAddress(p.Recipient)
		from, f := mail.ParseAddress(p.From)
		return e == nil && f == nil && to.Address == p.Recipient && from.Address == p.From && strings.HasPrefix(p.Secret, "re_") && !strings.ContainsAny(p.Secret, " \r\n\t")
	}
	return false
}
func AdminNotificationSettings(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	p, v, e := readNotificationConfig()
	if e != nil {
		localError(c, 503, "通知设置暂不可用")
		return
	}
	has := p.Secret != ""
	p.Secret = ""
	list := []gin.H{}
	rows, e := db.DB.Query("SELECT id,state,attempts,created_at,updated_at,error FROM notification_outbox ORDER BY created_at DESC LIMIT 50")
	if e != nil {
		localError(c, 503, "通知记录暂不可用")
		return
	}
	for rows.Next() {
		var id, state, msg string
		var attempts, at, updated int64
		if rows.Scan(&id, &state, &attempts, &at, &updated, &msg) == nil {
			list = append(list, gin.H{"id": id, "state": state, "attempts": attempts, "created_at": at, "updated_at": updated, "error": msg})
		}
	}
	rows.Close()
	c.JSON(200, gin.H{"settings": p, "version": v, "secret_configured": has, "records": list})
}
func AdminNotificationSave(c *gin.Context) {
	var req struct {
		Settings  notificationConfig `json:"settings"`
		Version   int64              `json:"version"`
		Confirmed bool               `json:"confirmed"`
		Clear     bool               `json:"clear_secret"`
	}
	if !localBody(c, &req) {
		return
	}
	old, _, e := readNotificationConfig()
	if e != nil {
		localError(c, 503, "通知配置暂不可用")
		return
	}
	if req.Clear {
		req.Settings.Secret = ""
	} else if req.Settings.Secret == "" && req.Settings.Channel == old.Channel {
		req.Settings.Secret = old.Secret
	}
	if !req.Confirmed || !validNotification(req.Settings) {
		localError(c, 400, "请确认通知授权并填写有效的接收地址和发送凭证；邮箱使用 Resend，手机使用 Telegram")
		return
	}
	raw, _ := json.Marshal(req.Settings)
	r, e := db.DB.Exec("UPDATE notification_config SET value=?,version=version+1 WHERE id=1 AND version=?", string(raw), req.Version)
	if e != nil {
		localError(c, 503, "保存通知失败")
		return
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		localError(c, 409, "通知配置已更新，请刷新重试")
		return
	}
	db.WriteAudit(c.GetString("username"), "notification_config", "updated; credentials omitted", c.ClientIP())
	AdminNotificationSettings(c)
}

const transientAlertGrace = 10 * time.Minute

func notificationAlertKind(key string) string {
	switch {
	case key == "money_inventory":
		return "卡片余额查询"
	case key == "money_pricing":
		return "Plus 费率查询"
	case key == "money_candidates":
		return "卡片可用性查询"
	case key == "money_fee":
		return "Plus 服务费限制"
	case key == "money" || strings.HasPrefix(key, "topup:"):
		return "自动资金操作"
	case key == "enrollment":
		return "新卡加入支付名单"
	case strings.HasPrefix(key, "order:"):
		return "订单状态核查"
	case strings.HasPrefix(key, "renewal:"):
		return "自动续费未取消"
	case strings.HasPrefix(key, "pro:") || key == "pro_restart":
		return "Pro 专卡处理"
	case strings.HasPrefix(key, "retire:"):
		return "卡片销卡"
	case key == "card_lifecycle" || key == "card_cycles":
		return "支付卡使用规则"
	case key == "card_remarks":
		return "上游卡片用途备注"
	case key == "code_reserve":
		return "备用卡密库存"
	case key == "finance-sync" || key == "finance-mismatch" || strings.HasPrefix(key, "finance-card:"):
		return "资金对账"
	case strings.HasPrefix(key, "operations:"):
		return "服务器运行检查"
	default:
		return "后台自动化"
	}
}

func notificationAlertObject(key string, localID int64) string {
	parseSuffix := func(prefix string) (int64, bool) {
		if !strings.HasPrefix(key, prefix) {
			return 0, false
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(key, prefix), 10, 64)
		return id, err == nil && id > 0
	}
	if id, ok := parseSuffix("topup:"); ok {
		return fmt.Sprintf("支付卡 #%d", id)
	}
	if id, ok := parseSuffix("retire:"); ok {
		return fmt.Sprintf("支付卡 #%d", id)
	}
	if id, ok := parseSuffix("finance-card:"); ok {
		return fmt.Sprintf("支付卡 #%d", id)
	}
	if localID > 0 {
		return fmt.Sprintf("站内订单 #%d", localID)
	}
	switch key {
	case "money", "money_inventory", "money_pricing", "money_candidates", "money_fee":
		return "自动补款与开卡任务"
	case "enrollment":
		return "已确认到账的新卡"
	case "code_reserve":
		return "各产品的备用卡密"
	case "card_lifecycle", "card_cycles":
		return "支付名单中的卡片"
	case "card_remarks":
		return "Zovo 名下卡片备注"
	case "finance-sync", "finance-mismatch":
		return "Zovo 钱包与卡片流水"
	}
	if strings.HasPrefix(key, "operations:") {
		return "兑换站后台服务"
	}
	return "兑换站后台任务"
}

func notificationAlertHandling(key string) string {
	switch {
	case strings.HasPrefix(key, "order:"):
		return "该订单只继续查单，不会重复付款；上游状态恢复一致后自动解除提醒。"
	case strings.HasPrefix(key, "renewal:"):
		return "保留已经确认的会员结果，只核查续费关闭状态，不重复付款。"
	case strings.HasPrefix(key, "pro:") || key == "pro_restart":
		return "订单保留在待核对状态；结果不明的开卡或付款不会自动重试。"
	case strings.HasPrefix(key, "retire:"):
		return "相关卡片停止进入新付款；销卡结果不明时不会重复提交销卡。"
	case key == "money" || strings.HasPrefix(key, "money_") || strings.HasPrefix(key, "topup:"):
		return "与异常条件有关的补款或开卡会被跳过或暂停；结果未确认时不会重复扣款。"
	case key == "enrollment":
		return "不会重复开卡；已确认到账的新卡保留，等待加入支付名单。"
	case key == "finance-sync" || key == "finance-mismatch" || strings.HasPrefix(key, "finance-card:"):
		return "不使用不完整流水作资金判断；后台继续分批同步和对账。"
	case key == "code_reserve":
		return "不会发放未确认入库的卡密；后台下一轮继续补足各产品库存。"
	case key == "card_lifecycle" || key == "card_cycles":
		return "卡片计次和销卡停在安全状态，不会因此盲目销卡或重复付款。"
	case key == "card_remarks":
		return "不会覆盖手工备注或重复付款；后台每 5 分钟重试，恢复后自动解除提醒。"
	case strings.HasPrefix(key, "operations:"):
		return "系统保留现有防重复和资金保护规则；异常恢复后自动解除提醒。"
	default:
		return "系统已记录异常，并保留防重复付款和防重复资金操作保护。"
	}
}

func notificationAlertAction(key, message string) string {
	if strings.Contains(message, "请") || strings.Contains(message, "人工") || strings.Contains(message, "联系管理员") {
		return "需要。请按“当前情况”中的说明核对；处理后系统会在下一轮检查中确认是否恢复。"
	}
	if transientNotificationAlert(key) || strings.Contains(message, "后台将继续") || strings.Contains(message, "稍后继续") || strings.Contains(message, "恢复后") {
		return "暂时不需要。系统会继续自动核查；如果条件恢复，提醒会自动解除。"
	}
	return "暂时不需要立即操作。系统会继续核查；若提醒持续存在，可按提醒编号在后台定位。"
}

func notificationStatusLabel(state string) string {
	switch state {
	case "inflight":
		return "请求提交中"
	case "pending":
		return "已受理，等待到账核查"
	case "unknown":
		return "结果不明，已停止自动重试"
	case "balance_verified":
		return "余额已确认"
	case "review":
		return "待人工核对"
	case "reserved":
		return "处理中"
	case "consumed", "completed":
		return "已完成"
	case "failed", "declined":
		return "已失败"
	default:
		return state
	}
}

func notificationAlertContext(key, message string, localID int64) []string {
	details := []string{}
	if localID > 0 {
		var plan, status string
		var cardID, upstreamID int64
		if err := db.DB.QueryRow("SELECT plan,status,card_id,upstream_id FROM local_cdks WHERE id=?", localID).Scan(&plan, &status, &cardID, &upstreamID); err == nil {
			details = append(details, "订单状态："+notificationStatusLabel(status))
			if plan != "" {
				details = append(details, "订单产品："+plan)
			}
			if cardID > 0 {
				details = append(details, fmt.Sprintf("支付卡：#%d", cardID))
			}
			if upstreamID > 0 {
				details = append(details, fmt.Sprintf("上游订单：#%d", upstreamID))
			}
		}
	}
	if key == "money" && (strings.Contains(message, "结果未确认") || strings.Contains(message, "结果不明") || strings.Contains(message, "超过 30 分钟") || strings.Contains(message, "接入密钥")) {
		var id, action, state string
		var cardID, resultCardID, amount, reserved int64
		err := db.DB.QueryRow(`SELECT id,action,card_id,result_card_id,amount_minor,reserved_minor,state
			FROM automation_money WHERE state IN ('inflight','pending','unknown') ORDER BY created_at DESC LIMIT 1`).Scan(&id, &action, &cardID, &resultCardID, &amount, &reserved, &state)
		if err == nil {
			actionName := map[string]string{"open": "自动开卡", "topup": "卡片补款", "pro_open": "Pro 专卡开卡"}[action]
			if actionName == "" {
				actionName = action
			}
			target := cardID
			if action == "open" || action == "pro_open" {
				target = resultCardID
			}
			details = append(details, "资金任务："+id, "资金操作："+actionName, fmt.Sprintf("操作金额：$%.2f；预算占用：$%.2f", float64(amount)/100, float64(reserved)/100), "任务状态："+notificationStatusLabel(state))
			if target > 0 {
				details = append(details, fmt.Sprintf("关联卡片：#%d", target))
			}
		}
	}
	return details
}

func detailedNotificationMessage(key, message string, localID, firstSeen, updatedAt int64) string {
	formatTime := func(stamp int64) string {
		return time.Unix(stamp, 0).In(operationsBangkok).Format("2006-01-02 15:04:05") + "（曼谷时间）"
	}
	lines := []string{
		"⚠️ 枫叶兑换站 · " + notificationAlertKind(key) + "异常",
		"",
		"首次发现：" + formatTime(firstSeen),
	}
	if updatedAt > firstSeen+5 {
		lines = append(lines, "最近确认："+formatTime(updatedAt))
	}
	lines = append(lines,
		"涉及对象："+notificationAlertObject(key, localID),
		"提醒编号："+key,
	)
	lines = append(lines, notificationAlertContext(key, message, localID)...)
	lines = append(lines,
		"",
		"当前情况："+strings.TrimSpace(message),
		"系统处理："+notificationAlertHandling(key),
		"需要你处理："+notificationAlertAction(key, message),
		"后台入口：https://cdk.maple1189ai.com/ops/automation",
	)
	return strings.Join(lines, "\n")
}

func transientNotificationAlert(key string) bool {
	return key == "finance-sync" || strings.HasPrefix(key, "finance-card:") ||
		key == "money_pricing" || key == "money_candidates" || key == "money_inventory" ||
		key == "card_remarks" ||
		key == "operations:wallet-query" || key == "operations:monitor"
}

// Track when the current alert text first appeared. Repeated scans keep the
// original time; resolution or changed text starts a new observation period.
func syncNotificationAlertState(now int64) error {
	if _, err := db.DB.Exec(`DELETE FROM notification_alert_state WHERE NOT EXISTS (
	 SELECT 1 FROM automation_alerts a WHERE a.alert_key=notification_alert_state.alert_key
	 AND a.resolved=0 AND a.message=notification_alert_state.message)`); err != nil {
		return err
	}
	if _, err := db.DB.Exec(`INSERT OR IGNORE INTO notification_alert_state(alert_key,message,first_seen)
	 SELECT alert_key,message,? FROM automation_alerts WHERE resolved=0`, now); err != nil {
		return err
	}
	_, err := db.DB.Exec(`UPDATE notification_outbox SET state='cancelled',error='异常已自动恢复，不再发送',updated_at=?
	 WHERE state IN ('pending','retry') AND id IN (
	  SELECT m.outbox_id FROM notification_alert_outbox m WHERE NOT EXISTS (
	   SELECT 1 FROM automation_alerts a WHERE a.alert_key=m.alert_key AND a.message=m.message AND a.resolved=0))`, now)
	return err
}

func actionableNotificationAlerts(now int64) int64 {
	if syncNotificationAlertState(now) != nil {
		return 0
	}
	rows, err := db.DB.Query(`SELECT a.alert_key,s.first_seen FROM automation_alerts a
	 JOIN notification_alert_state s ON s.alert_key=a.alert_key AND s.message=a.message WHERE a.resolved=0`)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var count int64
	for rows.Next() {
		var key string
		var first int64
		if rows.Scan(&key, &first) == nil && (!transientNotificationAlert(key) || first <= now-int64(transientAlertGrace/time.Second)) {
			count++
		}
	}
	return count
}

// Delivery remains responsive even while upstream order/finance checks time out.
func StartNotificationDispatcher(ctx context.Context) {
	go func() {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				notice, cancel := context.WithTimeout(ctx, 25*time.Second)
				dispatchNotifications(notice)
				cancel()
				timer.Reset(30 * time.Second)
			}
		}
	}()
}
func AdminNotificationTest(c *gin.Context) {
	var req struct {
		Confirmed bool `json:"confirmed"`
	}
	if !localBody(c, &req) {
		return
	}
	p, v, e := readNotificationConfig()
	if !req.Confirmed || e != nil || !p.Enabled || !validNotification(p) {
		localError(c, 400, "请先保存并启用有效通知配置，再确认发送测试")
		return
	}
	now := time.Now().Unix()
	id := localHash(fmt.Sprintf("test:%d:%d", v, now/60))
	_, e = db.DB.Exec("INSERT OR IGNORE INTO notification_outbox(id,config_version,created_at,updated_at,message) VALUES(?,?,?,?,?)", id, v, now, now, "这是一条兑换站测试通知。通知通道已收到测试请求。")
	if e != nil {
		localError(c, 503, "无法加入通知队列")
		return
	}
	db.WriteAudit(c.GetString("username"), "notification_test", "queued, maximum once per minute", c.ClientIP())
	c.JSON(200, gin.H{"message": "测试已排队，请查看发送记录；同一分钟不会重复排队"})
}

// Fixed HTTPS provider endpoints, no redirects, no caller-supplied callback URLs.
// Never log response bodies or errors containing Telegram's token-bearing URL.
var notificationHTTP = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func sendNotification(ctx context.Context, p notificationConfig, id, message string) (string, string) {
	endpoint := "https://api.resend.com/emails"
	payload := map[string]any{"from": p.From, "to": []string{p.Recipient}, "subject": "兑换站异常提醒", "text": message}
	if p.Channel == "telegram" {
		endpoint = "https://api.telegram.org/bot" + p.Secret + "/sendMessage"
		payload = map[string]any{"chat_id": p.Recipient, "text": message}
	}
	raw, _ := json.Marshal(payload)
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if e != nil {
		return "failed", "通知请求无效"
	}
	req.Header.Set("Content-Type", "application/json")
	if p.Channel == "email" {
		req.Header.Set("Authorization", "Bearer "+p.Secret)
		req.Header.Set("Idempotency-Key", id)
	}
	res, e := notificationHTTP.Do(req)
	if e != nil {
		return "unknown", "发送结果不明；为避免重复通知，不自动重发"
	}
	defer res.Body.Close()
	body, e := io.ReadAll(io.LimitReader(res.Body, 16384))
	if e != nil {
		return "unknown", "无法确认通知平台响应，不自动重发"
	}
	if res.StatusCode == 429 {
		return "retry", "通知平台限流，稍后限次重试"
	}
	if res.StatusCode >= 500 {
		return "unknown", "通知平台异常，发送结果不明，不自动重发"
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "failed", "通知平台拒绝请求，请核对发送凭证、接收地址和权限"
	}
	var response struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &response) != nil {
		return "unknown", "通知平台响应格式无法确认"
	}
	if p.Channel == "telegram" && !response.OK {
		return "failed", "Telegram 未确认接收"
	}
	if p.Channel == "email" && response.ID == "" {
		return "unknown", "邮件平台未返回发送编号"
	}
	return "accepted", "平台已接收，不代表接收人已阅读或邮件已投递"
}
func dispatchNotifications(ctx context.Context) {
	now := time.Now().Unix()
	db.DB.Exec("UPDATE notification_outbox SET state='unknown',error='服务重启或发送超时，结果不明，不自动重发' WHERE state='sending' AND updated_at<?", now-600)
	p, v, e := readNotificationConfig()
	if e != nil {
		return
	}
	db.DB.Exec("UPDATE notification_outbox SET state='cancelled',error='配置已更改，旧通知不发送到新地址',updated_at=? WHERE state IN ('pending','retry') AND config_version<>?", now, v)
	if !p.Enabled || !validNotification(p) {
		return
	}
	if syncNotificationAlertState(now) != nil {
		return
	}
	rows, e := db.DB.Query(`SELECT a.alert_key,a.message,a.local_id,s.first_seen,a.updated_at FROM automation_alerts a
	 JOIN notification_alert_state s ON s.alert_key=a.alert_key AND s.message=a.message
	 WHERE a.resolved=0 ORDER BY a.updated_at DESC LIMIT 100`)
	if e != nil {
		return
	}
	type alertNotice struct {
		key, message, alertKey, sourceMessage string
		localID, firstSeen, updatedAt         int64
	}
	notices := []alertNotice{}
	for rows.Next() {
		var key, msg string
		var localID, firstSeen, updatedAt int64
		if rows.Scan(&key, &msg, &localID, &firstSeen, &updatedAt) == nil {
			if transientNotificationAlert(key) && firstSeen > now-int64(transientAlertGrace/time.Second) {
				continue
			}
			identity := fmt.Sprintf("detail-v2|%d|%s|%s", v, key, msg)
			notices = append(notices, alertNotice{key: localHash(identity), alertKey: key, sourceMessage: msg, localID: localID, firstSeen: firstSeen, updatedAt: updatedAt})
		}
	}
	rows.Close()
	for i := range notices {
		notices[i].message = detailedNotificationMessage(notices[i].alertKey, notices[i].sourceMessage, notices[i].localID, notices[i].firstSeen, notices[i].updatedAt)
	}
	for _, notice := range notices {
		tx, err := db.DB.Begin()
		if err != nil {
			continue
		}
		r, err := tx.Exec("INSERT OR IGNORE INTO notification_outbox(id,config_version,created_at,updated_at,message) VALUES(?,?,?,?,?)", notice.key, v, now, now, notice.message)
		if err == nil {
			if n, _ := r.RowsAffected(); n == 1 {
				_, err = tx.Exec("INSERT INTO notification_alert_outbox(outbox_id,alert_key,message) VALUES(?,?,?)", notice.key, notice.alertKey, notice.sourceMessage)
			}
		}
		if err != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}
	// At most one external message per worker cycle; retries are only for explicit 429.
	var id, msg string
	var attempts int
	if db.DB.QueryRow("SELECT id,message,attempts FROM notification_outbox WHERE config_version=? AND state IN ('pending','retry') AND next_run<=? AND attempts<3 ORDER BY created_at,id LIMIT 1", v, now).Scan(&id, &msg, &attempts) != nil {
		return
	}
	r, e := db.DB.Exec("UPDATE notification_outbox SET state='sending',attempts=attempts+1,updated_at=? WHERE id=? AND state IN ('pending','retry') AND config_version=(SELECT version FROM notification_config WHERE id=1)", now, id)
	if e != nil {
		return
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return
	}
	state, reason := sendNotification(ctx, p, id, msg)
	if state == "retry" && attempts >= 2 {
		state = "failed"
		reason = "已达到 3 次限流重试上限，请检查通知平台"
	}
	db.DB.Exec("UPDATE notification_outbox SET state=?,error=?,updated_at=?,next_run=? WHERE id=?", state, reason, time.Now().Unix(), now+900, id)
}
