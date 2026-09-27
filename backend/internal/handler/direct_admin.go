package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func directReadError(c *gin.Context, e error) {
	var upstream *cardplatform.APIError
	if errors.As(e, &upstream) {
		switch upstream.HTTPStatus {
		case 401, 403:
			localError(c, 502, "Zovo 拒绝访问，请检查接入密钥、权限或服务器白名单")
			return
		case 404:
			localError(c, 404, "找不到本 API 账号可访问的订单")
			return
		case 429:
			localError(c, 429, "上游请求频繁，请稍后刷新")
			return
		}
	}
	localError(c, 502, "暂时无法读取 Zovo 订单，请稍后刷新；没有发起付款")
}
func AdminDirectOrders(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	page, e := strconv.Atoi(c.DefaultQuery("page", "1"))
	if e != nil || page < 1 || page > 10000 {
		localError(c, 400, "页码不正确")
		return
	}
	data, e := cardplatform.NewFromSettings().DirectOrderList(c.Request.Context(), page)
	if e != nil {
		directReadError(c, e)
		return
	}
	// Zovo's list endpoint intentionally returns only orders created by the
	// current API user. Merge signed webhook facts separately so web-console
	// upgrades and orders from an older key still appear without pretending
	// they can be operated through the current key.
	apiIDs := map[int64]bool{}
	if list, ok := data["list"].([]map[string]any); ok {
		for _, order := range list {
			if id := anyToInt64(order["id"]); id > 0 {
				apiIDs[id] = true
			}
		}
	}
	synced := []map[string]any{}
	seen := map[int64]bool{}
	if events, err := db.ListDirectOrderWebhookEvents(1000); err == nil {
		for _, event := range events {
			var payload map[string]any
			if json.Unmarshal([]byte(event.Payload), &payload) != nil {
				continue
			}
			id := anyToInt64(payload["order_id"])
			if id <= 0 {
				id = anyToInt64(payload["id"])
			}
			if id <= 0 || apiIDs[id] || seen[id] {
				continue
			}
			seen[id] = true
			payload["id"] = float64(id)
			if _, ok := payload["status"]; !ok && event.EventType == "gpt_direct.completed" {
				payload["status"] = "completed"
			}
			if _, ok := payload["created_at"]; !ok {
				payload["created_at"] = event.CreatedAt
			}
			order := cardplatform.PublicDirectOrder(payload)
			order["id"] = id
			order["synced_only"] = true
			order["source"] = "zovo_webhook"
			synced = append(synced, order)
			if len(synced) >= 100 {
				break
			}
		}
	}
	// The local redemption ledger is durable evidence for submitted upstream
	// orders. It fills the historical gap left by API-key scoping and webhook
	// events that only started arriving after the integration was configured.
	if facts, err := db.ListLocalDirectOrderFacts(1000); err == nil {
		for _, fact := range facts {
			id := fact.UpstreamID
			if id <= 0 || apiIDs[id] || seen[id] {
				continue
			}
			seen[id] = true
			status := strings.TrimSpace(fact.Status)
			switch status {
			case "consumed":
				status = "completed"
			case "reserved":
				status = "running"
			case "review":
				status = "requires_action"
			case "unused", "disabled":
				continue
			}
			created := fact.CreatedAt
			if fact.ActivatedAt > 0 {
				created = fact.ActivatedAt
			}
			order := map[string]any{
				"id": id, "client_request_id": fact.RequestID, "product": "gpt", "plan": fact.Plan,
				"status": status, "account_email": fact.Email, "card_id": fact.CardID,
				"created_at": created, "synced_only": true, "source": "cdk_redemption", "local_redemption_id": fact.LocalID,
			}
			if fact.ActivatedAt > 0 {
				order["completed_at"] = fact.ActivatedAt
			}
			synced = append(synced, order)
			if len(synced) >= 1000 {
				break
			}
		}
	}
	data["synced"] = synced
	data["synced_total"] = len(synced)
	c.JSON(200, data)
}
func directID(c *gin.Context) (int64, bool) {
	id, e := strconv.ParseInt(c.Param("id"), 10, 64)
	if e != nil || id <= 0 {
		localError(c, 400, "订单编号不正确")
		return 0, false
	}
	return id, true
}
func AdminDirectDetail(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, ok := directID(c)
	if !ok {
		return
	}
	data, e := cardplatform.NewFromSettings().DirectOrderDetail(c.Request.Context(), id)
	if e != nil {
		directReadError(c, e)
		return
	}
	c.JSON(200, data)
}
func directActionAllowed(order map[string]any, action string) bool {
	status, _ := order["status"].(string)
	if action == "cancel" {
		return status == "queued" || status == "awaiting_card" || status == "funding_pending"
	}
	renewal, _ := order["renewal_status"].(string)
	return action == "cancel-renewal" && status == "completed" && (order["product"] == nil || order["product"] == "gpt") && (renewal == "pending" || renewal == "warning")
}
func AdminDirectAction(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, ok := directID(c)
	if !ok {
		return
	}
	action := c.Param("action")
	if action != "cancel" && action != "cancel-renewal" {
		localError(c, 404, "操作不存在")
		return
	}
	var req struct {
		Confirmed      bool   `json:"confirmed"`
		ExpectedStatus string `json:"expected_status"`
	}
	if !localBody(c, &req) {
		return
	}
	if !req.Confirmed || req.ExpectedStatus == "" {
		localError(c, 400, "请先查看订单并确认操作")
		return
	}
	cli := cardplatform.NewFromSettings()
	data, e := cli.DirectOrderDetail(c.Request.Context(), id)
	if e != nil {
		directReadError(c, e)
		return
	}
	order := data["order"].(map[string]any)
	if order["status"] != req.ExpectedStatus || !directActionAllowed(order, action) {
		localError(c, 409, "当前订单状态不允许此操作，请刷新详情。已取消续费的订单无需重复取消。")
		return
	}
	cfg := cardplatform.LoadConfig()
	key := localHash(cfg.SiteBase + "|" + cfg.APIKey + "|" + strconv.FormatInt(id, 10) + "|" + action)
	now := time.Now().Unix()
	// Durable single-flight: crash/timeout leaves an unknown attempt blocked across restarts.
	result, e := db.DB.Exec(`INSERT INTO direct_admin_actions(action_key,attempted_at,state) VALUES(?,?,'inflight')
 ON CONFLICT(action_key) DO UPDATE SET attempted_at=excluded.attempted_at,state='inflight'
 WHERE direct_admin_actions.state='done' AND direct_admin_actions.attempted_at<?`, key, now, now-120)
	if e != nil {
		localError(c, 503, "操作记录暂不可用，未提交取消请求")
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		localError(c, 409, "该操作正在处理、结果待核对或距离上次操作不足两分钟。请先刷新；结果不明时在 Zovo 核对，不要连续重试。")
		return
	}
	db.WriteAudit(c.GetString("username"), "direct_"+action, fmt.Sprintf("order=%d submitted", id), c.ClientIP())
	reply, e := cli.DirectAdminAction(c.Request.Context(), id, action)
	state := "done"
	if e != nil {
		state = "unknown"
	}
	_, saveErr := db.DB.Exec("UPDATE direct_admin_actions SET state=? WHERE action_key=?", state, key)
	if e != nil || saveErr != nil {
		localError(c, 502, "取消请求结果尚未确认，请刷新订单或到 Zovo 核对。本站不会自动重试，也不会重新付款。")
		return
	}
	db.WriteAudit(c.GetString("username"), "direct_"+action, fmt.Sprintf("order=%d response_received", id), c.ClientIP())
	c.JSON(200, gin.H{"result": reply, "message": "已收到上游响应，请刷新详情确认最终状态；取消续费不等于退款。"})
}
