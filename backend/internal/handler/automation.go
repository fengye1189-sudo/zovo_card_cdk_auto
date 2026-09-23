package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

type automationPolicy struct {
	Sync         bool   `json:"sync_enabled"`
	Paused       bool   `json:"paused"`
	Renewal      bool   `json:"renewal_enabled"`
	Topup        bool   `json:"topup_enabled"`
	Open         bool   `json:"open_enabled"`
	Retire       bool   `json:"retire_enabled"`
	AutoProduct  bool   `json:"auto_product_enabled"`
	DailyBudget  int64  `json:"daily_budget_minor"`
	MaxOperation int64  `json:"max_operation_minor"`
	WalletFloor  int64  `json:"wallet_floor_minor"`
	Threshold    int64  `json:"threshold_minor"`
	Target       int64  `json:"target_minor"`
	CardCeiling  int64  `json:"card_ceiling_minor"`
	InitAmount   int64  `json:"init_amount_minor"`
	DailyOpen    int64  `json:"daily_open_limit"`
	Product      string `json:"product_code"`
	First        string `json:"first_name"`
	Last         string `json:"last_name"`
	Enroll       bool   `json:"enroll_created_cards"`
}

func readAutomationPolicy() (automationPolicy, int64, error) {
	var p automationPolicy
	var raw string
	var version int64
	e := db.DB.QueryRow("SELECT value,version FROM automation_policy WHERE id=1").Scan(&raw, &version)
	if e != nil {
		return p, 0, e
	}
	e = json.Unmarshal([]byte(raw), &p)
	return p, version, e
}
func automationBlocked() bool {
	p, _, e := readAutomationPolicy()
	if e != nil || p.Paused {
		return true
	}
	var n int
	e = db.DB.QueryRow("SELECT COUNT(*) FROM automation_money WHERE state IN ('inflight','unknown','pending')").Scan(&n)
	return e != nil || n > 0
}
func validateAutomation(p automationPolicy) bool {
	for _, n := range []int64{p.DailyBudget, p.MaxOperation, p.WalletFloor, p.Threshold, p.Target, p.CardCeiling, p.InitAmount, p.DailyOpen} {
		if n < 0 || n > 100000000 {
			return false
		}
	}
	if len(p.Product) > 80 || len(p.First) > 80 || len(p.Last) > 80 {
		return false
	}
	if (p.Renewal || p.Topup || p.Open || p.Retire) && !p.Sync {
		return false
	}
	if p.Topup || p.Open {
		if p.DailyBudget <= 0 || p.MaxOperation <= 0 || p.MaxOperation > p.DailyBudget || p.CardCeiling <= 0 {
			return false
		}
	}
	if p.Topup && (p.Threshold <= 0 || p.Target <= p.Threshold || p.Target > p.CardCeiling) {
		return false
	}
	if p.Open && (p.InitAmount <= 0 || p.InitAmount > p.CardCeiling || p.DailyOpen < 1 || p.DailyOpen > 20 || (!p.AutoProduct && strings.TrimSpace(p.Product) == "") || strings.TrimSpace(p.First) == "" || strings.TrimSpace(p.Last) == "") {
		return false
	}
	return true
}
func AdminAutomationSettings(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	p, v, e := readAutomationPolicy()
	if e != nil {
		localError(c, 503, "自动化设置暂不可用")
		return
	}
	c.JSON(200, gin.H{"settings": p, "version": v})
}
func AdminAutomationSave(c *gin.Context) {
	var req struct {
		Settings  automationPolicy `json:"settings"`
		Version   int64            `json:"version"`
		Confirmed bool             `json:"confirmed"`
	}
	if !localBody(c, &req) {
		return
	}
	if !validateAutomation(req.Settings) || !req.Confirmed {
		localError(c, 400, "请确认自动化规则；开启资金操作必须设置正数预算、金额上限及必要的开卡资料")
		return
	}
	raw, _ := json.Marshal(req.Settings)
	r, e := db.DB.Exec("UPDATE automation_policy SET value=?,version=version+1 WHERE id=1 AND version=?", string(raw), req.Version)
	if e != nil {
		localError(c, 503, "保存失败")
		return
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		localError(c, 409, "设置已被更新，请刷新后再保存")
		return
	}
	db.WriteAudit(c.GetString("username"), "automation_settings", "updated with confirmation", c.ClientIP())
	AdminAutomationSettings(c)
}
func AdminAutomationPause(c *gin.Context) {
	// Independent emergency stop: never restores any other switch or clears unknown operations.
	for i := 0; i < 3; i++ {
		p, v, e := readAutomationPolicy()
		if e != nil {
			break
		}
		p.Paused = true
		raw, _ := json.Marshal(p)
		r, e := db.DB.Exec("UPDATE automation_policy SET value=?,version=version+1 WHERE id=1 AND version=?", string(raw), v)
		if e == nil {
			n, _ := r.RowsAffected()
			if n == 1 {
				db.WriteAudit(c.GetString("username"), "automation_pause", "new actions stopped", c.ClientIP())
				AdminAutomationSettings(c)
				return
			}
		}
	}
	localError(c, 503, "暂停未确认，请重试并检查充值开关")
}
func autoAlert(key string, id int64, message string) {
	_, _ = db.DB.Exec("INSERT INTO automation_alerts(alert_key,local_id,message,updated_at,resolved) VALUES(?,?,?,?,0) ON CONFLICT(alert_key) DO UPDATE SET message=excluded.message,updated_at=excluded.updated_at,resolved=0", key, id, message, time.Now().Unix())
}
func autoResolve(key string) {
	_, _ = db.DB.Exec("UPDATE automation_alerts SET resolved=1 WHERE alert_key=?", key)
}
func autoOrderKey(id int64) string { return "order:" + strconv.FormatInt(id, 10) }

// Runs inside the existing server, not inside a browser. Each claim survives restarts.
func StartAutomation(ctx context.Context) {
	if err := MaintainCodeReserve(); err != nil {
		autoAlert("code_reserve", 0, "备用卡密初始化失败，请检查服务配置。")
	}
	recoverInterruptedProOrders()
	recoverInterruptedPro5xReserve()
	// Persist cycle assignments at startup so monitoring and selection share the
	// same limits. This is local bookkeeping only; it makes no upstream call.
	if _, _, err := localPoolCards(readLocalSettings(), time.Now().Unix()); err != nil {
		autoAlert("card_cycles", 0, "支付卡周期初始化失败，新的 Plus/Go 付款将保持阻止状态；请检查数据库。")
	} else {
		autoResolve("card_cycles")
	}
	// A restart may follow a previously failed or interrupted sync whose retry
	// deadline is still in the future. Wake one immediate reconciliation so the
	// current process verifies upstream remarks before returning to the cadence.
	scheduleUpstreamCardRemarkSync()
	startUpstreamCardRemarkSync(ctx)
	go func() {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				runAutomationCycle(ctx)
				extra, cancel := context.WithTimeout(ctx, 90*time.Second)
				syncFinance(extra)
				cancel()
				timer.Reset(30 * time.Second)
			}
		}
	}()
}
func runAutomationCycle(ctx context.Context) {
	// Completion notices are durable local bookkeeping and must continue to
	// retry even when new payment automation is paused or sync is disabled.
	defer dispatchMarketplaceCompletionOutbox(ctx)
	if err := MaintainCodeReserve(); err != nil {
		autoAlert("code_reserve", 0, "备用卡密补充失败，请检查服务配置。")
	} else {
		autoResolve("code_reserve")
	}
	_, _ = db.DB.Exec("UPDATE automation_runtime SET heartbeat=? WHERE id=1", time.Now().Unix())
	p, _, e := readAutomationPolicy()
	if e != nil || !p.Sync {
		return
	}
	now := time.Now().Unix()
	_, e = db.DB.Exec("INSERT OR IGNORE INTO automation_watch(local_id,first_seen) SELECT id,? FROM local_cdks WHERE request_id<>''", now)
	if e != nil {
		return
	}
	rows, e := db.DB.Query("SELECT local_id FROM automation_watch WHERE next_check<=? ORDER BY next_check,local_id LIMIT 10", now)
	if e != nil {
		return
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		checkCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		watchOrder(checkCtx, id)
		cancel()
	}
	if ctx.Err() == nil {
		moneyCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		maintainAutomationCards(moneyCtx)
		cancel()
	}
}
func watchOrder(ctx context.Context, id int64) {
	now := time.Now().Unix()
	claim, e := db.DB.Exec("UPDATE automation_watch SET next_check=? WHERE local_id=? AND next_check<=?", now+300, id, now)
	if e != nil {
		return
	}
	n, _ := claim.RowsAffected()
	if n != 1 {
		return
	}
	key := autoOrderKey(id)
	r, e := loadLocal("id", strconv.FormatInt(id, 10))
	if e != nil {
		return
	}
	cli := cardplatform.NewFromSettings()
	if r.Upstream == 0 {
		var page int
		_ = db.DB.QueryRow("SELECT scan_page FROM automation_watch WHERE local_id=?", id).Scan(&page)
		if page < 1 {
			page = 1
		}
		// Read-only recovery uses the original merchant ID; it never resubmits a payment.
		found, total, e := cli.FindDirectOrder(ctx, r.Request, page)
		if e == nil && found > 0 {
			_, e = db.DB.Exec("UPDATE local_cdks SET upstream_id=? WHERE id=? AND request_id=? AND upstream_id=0", found, id, r.Request)
			if e == nil {
				r.Upstream = found
			}
		}
		if r.Upstream == 0 {
			next := page + 1
			if page*20 >= total || next > 10000 {
				next = 1
			}
			_, _ = db.DB.Exec("UPDATE automation_watch SET scan_page=?,next_check=? WHERE local_id=?", next, now+300, id)
			autoAlert(key, id, "订单缺少上游编号，后台正在按原商户订单号查找；不会再次付款。仍查不到时请在 Zovo 核对。")
			return
		}
	}
	data, e := cli.DirectBoundOrderDetail(ctx, r.Upstream, r.Request)
	if e != nil {
		_, _ = db.DB.Exec("UPDATE automation_watch SET failures=failures+1,next_check=? WHERE local_id=?", now+300, id)
		autoAlert(key, id, "暂时无法读取上游订单，后台将继续查询；未重试付款。")
		return
	}
	order := data["order"].(map[string]any)
	autoResolve(key)
	state, _ := order["status"].(string)
	renewal, _ := order["renewal_status"].(string)
	if r.Status == "consumed" && state != "completed" {
		autoAlert(key, id, "已完成订单的上游状态出现矛盾，保留已核销状态，请人工核对。")
		return
	}
	localState, message := r.Status, r.Message
	switch state {
	case "completed":
		localState, message = "consumed", "会员已开通，卡密已核销"
	case "failed_precharge", "cancelled":
		localState, message = "failed", "升级未完成，该卡密已永久锁定，不能再次兑换。请点击联系客服处理。"
	case "declined", "failed", "requires_action":
		localState, message = "review", "订单需要商家核对，请勿重复付款"
	}
	var statusErr error
	if localState == "consumed" {
		completedAt, _ := order["completed_at"].(string)
		statusErr = recordAuthoritativeLocalCompletion(id, message, completedAt, now)
	} else {
		statusErr = recordAuthoritativeLocalStatus(id, localState, message, now)
	}
	if statusErr != nil {
		_, _ = db.DB.Exec("UPDATE automation_watch SET failures=failures+1,next_check=? WHERE local_id=?", now+300, id)
		autoAlert(key, id, "订单状态已读取，但本站卡片周期记录未能原子更新；未计次、未重试付款，后台稍后继续核对。")
		return
	}
	if err := recordAutomationOutcome(id, r.CardID, state, now); err != nil {
		autoAlert("card_lifecycle", 0, "卡片成功率记录暂时失败；不会因此销卡，后台稍后继续核对。")
	} else if err := recordAutomationDecline(id, r.CardID, state, now); err != nil {
		autoAlert("card_lifecycle", 0, "卡片拒付次数记录暂时失败；不会因此销卡，后台稍后继续核对。")
	} else {
		autoResolve("card_lifecycle")
	}
	_, _ = db.DB.Exec("UPDATE local_cdks SET last_checked=? WHERE id=? AND request_id=? AND upstream_id=?", now, id, r.Request, r.Upstream)
	snapshot, _ := json.Marshal(data)
	next := now + 60
	renewalConfirmed := renewalCancellationConfirmed(order)
	if state == "completed" {
		// Subscription cancellation is asynchronous upstream. Recheck quickly
		// while it settles, but keep the mutation retry lock at 15 minutes.
		next = now + 60
		if renewalConfirmed {
			next = now + 86400
		}
	}
	if state == "declined" || state == "failed_precharge" || state == "failed" || state == "cancelled" {
		next = now + 3600
	}
	_, _ = db.DB.Exec("UPDATE automation_watch SET status=?,renewal=?,checked_at=?,next_check=?,failures=0,snapshot=? WHERE local_id=?", state, renewal, now, next, string(snapshot), id)
	var first int64
	_ = db.DB.QueryRow("SELECT first_seen FROM automation_watch WHERE local_id=?", id).Scan(&first)
	if localState == "failed" {
		r.Status, r.Message = localState, message
		notifyLocalFailure(ctx, key, r, state, true)
	} else if localState == "review" {
		r.Status, r.Message = localState, message
		notifyLocalFailure(ctx, key, r, state, false)
	} else if state != "completed" && now-first > 1800 {
		autoAlert(key, id, "订单超过 30 分钟未完成，后台继续查单，请检查账号验证或上游进度。")
	}
	p, _, e := readAutomationPolicy()
	automaticCancellation := e == nil && p.Renewal && !p.Paused && directActionAllowed(order, "cancel-renewal")
	if automaticCancellation {
		autoCancelRenewal(ctx, cli, id, r.Upstream)
	}
	renewalKey := "renewal:" + strconv.FormatInt(id, 10)
	if state == "completed" && !renewalConfirmed {
		var attempts int
		_ = db.DB.QueryRow("SELECT renewal_attempts FROM automation_watch WHERE local_id=?", id).Scan(&attempts)
		var activatedAt int64
		_ = db.DB.QueryRow("SELECT activated_at FROM local_cdks WHERE id=?", id).Scan(&activatedAt)
		if renewalStatusAwaitingUpstream(renewal) && activatedAt > 0 && now-activatedAt < 300 {
			// A completed order can briefly omit renewal_status before the
			// cancellation result is published. This is normal propagation, not
			// an exception worth sending to Telegram.
			autoResolve(renewalKey)
		} else if automaticCancellation && attempts < 3 {
			// A submitted cancellation is not yet proof, but it is not an
			// administrator-facing exception either. Wait for safe verification.
			autoResolve(renewalKey)
		} else if automaticCancellation {
			autoAlert(renewalKey, id, "会员已开通，但自动续费仍未取消；系统已完成限次处理仍未确认，请核对上游续费状态。")
		} else if renewalStatusAwaitingUpstream(renewal) {
			autoAlert(renewalKey, id, "会员已开通，但上游超过 5 分钟仍未返回取消续费结果；请核对上游续费状态。")
		} else {
			autoAlert(renewalKey, id, "会员已开通，但自动续费仍未取消；当前无法自动处理，请核对上游续费状态。")
		}
	} else {
		autoResolve(renewalKey)
	}
}

// The upstream uses not_requested for products that have no renewal to cancel.
// Neither that state nor a successful cancellation is an exception worth notifying.
func renewalCancellationOutstanding(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "success", "not_requested":
		return false
	default:
		return true
	}
}

func renewalCancellationConfirmed(order map[string]any) bool {
	for _, key := range []string{"will_renew", "subscription_will_renew"} {
		if value, ok := order[key].(bool); ok {
			return !value
		}
	}
	status, _ := order["renewal_status"].(string)
	return !renewalCancellationOutstanding(status)
}

func renewalStatusAwaitingUpstream(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "unknown", "processing":
		return true
	default:
		return false
	}
}

func autoCancelRenewal(ctx context.Context, cli *cardplatform.Client, localID, orderID int64) {
	now := time.Now().Unix()
	claim, e := db.DB.Exec("UPDATE automation_watch SET renewal_attempts=renewal_attempts+1,renewal_after=? WHERE local_id=? AND renewal_attempts<3 AND renewal_after<=?", now+900, localID, now)
	if e != nil {
		return
	}
	n, _ := claim.RowsAffected()
	if n != 1 {
		return
	}
	p, _, e := readAutomationPolicy()
	if e != nil || p.Paused || !p.Renewal {
		return
	}
	cfg := cardplatform.LoadConfig()
	key := localHash(cfg.SiteBase + "|" + cfg.APIKey + "|" + strconv.FormatInt(orderID, 10) + "|cancel-renewal")
	lock, e := db.DB.Exec("INSERT INTO direct_admin_actions(action_key,attempted_at,state) VALUES(?,?,'inflight') ON CONFLICT(action_key) DO UPDATE SET attempted_at=excluded.attempted_at,state='inflight' WHERE direct_admin_actions.state='done' AND direct_admin_actions.attempted_at<?", key, now, now-900)
	if e != nil {
		return
	}
	n, _ = lock.RowsAffected()
	if n != 1 {
		return
	}
	_, e = cli.DirectAdminAction(ctx, orderID, "cancel-renewal")
	state := "done"
	if e != nil {
		state = "unknown"
	}
	_, _ = db.DB.Exec("UPDATE direct_admin_actions SET state=? WHERE action_key=?", state, key)
	db.WriteAudit("automation", "cancel_renewal", fmt.Sprintf("order=%d result=%s", orderID, state), "")
	// A response is not proof of cancellation. The next GET confirms renewal_status.
}
func AdminAutomationStatus(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var heartbeat int64
	if e := db.DB.QueryRow("SELECT heartbeat FROM automation_runtime WHERE id=1").Scan(&heartbeat); e != nil {
		localError(c, 503, "状态暂不可用")
		return
	}
	alerts := []gin.H{}
	rows, e := db.DB.Query("SELECT alert_key,local_id,message,updated_at FROM automation_alerts WHERE resolved=0 ORDER BY updated_at DESC LIMIT 100")
	if e != nil {
		localError(c, 503, "读取提醒失败")
		return
	}
	for rows.Next() {
		var key, msg string
		var id, at int64
		if rows.Scan(&key, &id, &msg, &at) == nil {
			alerts = append(alerts, gin.H{"key": key, "local_id": id, "message": msg, "updated_at": at})
		}
	}
	rows.Close()
	watches := []gin.H{}
	rows, e = db.DB.Query("SELECT w.local_id,c.upstream_id,w.status,w.renewal,w.checked_at,w.next_check,w.renewal_attempts FROM automation_watch w JOIN local_cdks c ON c.id=w.local_id ORDER BY w.local_id DESC LIMIT 100")
	if e != nil {
		localError(c, 503, "读取跟进记录失败")
		return
	}
	for rows.Next() {
		var id, up, at, next, attempt int64
		var status, renewal string
		if rows.Scan(&id, &up, &status, &renewal, &at, &next, &attempt) == nil {
			watches = append(watches, gin.H{"local_id": id, "upstream_id": up, "status": status, "renewal": renewal, "checked_at": at, "next_check": next, "renewal_attempts": attempt})
		}
	}
	rows.Close()
	operations := []gin.H{}
	rows, e = db.DB.Query("SELECT id,action,card_id,amount_minor,reserved_minor,state,created_at,result_card_id FROM automation_money ORDER BY created_at DESC LIMIT 100")
	if e == nil {
		for rows.Next() {
			var id, action, state string
			var card, amount, cost, at, result int64
			if rows.Scan(&id, &action, &card, &amount, &cost, &state, &at, &result) == nil {
				operations = append(operations, gin.H{"id": id, "action": action, "card_id": card, "amount_minor": amount, "reserved_minor": cost, "state": state, "created_at": at, "result_card_id": result})
			}
		}
		rows.Close()
	}
	lifecycle := []gin.H{}
	rows, e = db.DB.Query(`SELECT l.card_id,l.product_code,l.bin,COALESCE(p.card_type,''),l.phase,l.decline_count,l.retire_state,l.retire_reason,
	 COALESCE(c.success_count,0),COALESCE(c.success_limit,0),COALESCE(c.cooldown_until,0),l.updated_at
	 FROM automation_card_lifecycle l LEFT JOIN local_card_cycles c ON c.card_id=l.card_id
	 LEFT JOIN automation_product_catalog p ON p.product_code=l.product_code
	 ORDER BY CASE l.retire_state WHEN 'unknown' THEN 0 WHEN 'queued' THEN 1 WHEN 'inflight' THEN 2 WHEN 'active' THEN 3 ELSE 4 END,l.updated_at DESC LIMIT 100`)
	if e == nil {
		for rows.Next() {
			var product, bin, cardType, phase, retireState, reason string
			var cardID, declines, successes, limit, cooldown, updated int64
			if rows.Scan(&cardID, &product, &bin, &cardType, &phase, &declines, &retireState, &reason, &successes, &limit, &cooldown, &updated) == nil {
				lifecycle = append(lifecycle, gin.H{"card_id": cardID, "product_code": product, "bin": bin, "card_type": cardType, "card_type_label": automationCardTypeLabel(cardType), "phase": phase, "decline_count": declines, "retire_state": retireState, "retire_reason": reason, "success_count": successes, "success_limit": limit, "cooldown_until": cooldown, "updated_at": updated})
			}
		}
		rows.Close()
	}
	productStats := []gin.H{}
	rows, e = db.DB.Query(`SELECT GROUP_CONCAT(DISTINCT d.product_code),MAX(d.bin),COALESCE(c.card_type,''),SUM(d.successes),SUM(d.declines),SUM(d.attempts),MAX(d.day)
	 FROM automation_product_daily d LEFT JOIN automation_product_catalog c ON c.product_code=d.product_code
	 WHERE d.day>=strftime('%Y-%m-%d','now','+7 hours','-29 days')
	 GROUP BY COALESCE(c.card_type,''),CASE WHEN TRIM(d.bin)<>'' THEN 'bin:'||TRIM(d.bin) ELSE 'product:'||d.product_code END
	 ORDER BY (SUM(successes)+1.0)/(SUM(attempts)+2.0) DESC,
	 SUM(successes) DESC,SUM(attempts) DESC,SUM(declines),GROUP_CONCAT(DISTINCT d.product_code)`)
	if e == nil {
		for rows.Next() {
			var product, bin, cardType, latest string
			var success, decline, attempts int64
			if rows.Scan(&product, &bin, &cardType, &success, &decline, &attempts, &latest) == nil {
				rate := float64(0)
				if attempts > 0 {
					rate = float64(success) / float64(attempts)
				}
				productStats = append(productStats, gin.H{"product_code": product, "bin": bin, "card_type": cardType, "card_type_label": automationCardTypeLabel(cardType), "successes": success, "declines": decline, "attempts": attempts, "success_rate": rate, "latest_day": latest})
			}
		}
		rows.Close()
	}
	c.JSON(200, gin.H{"heartbeat": heartbeat, "alerts": alerts, "orders": watches, "operations": operations, "card_lifecycle": lifecycle, "product_stats": productStats, "blocked": automationBlocked()})
}

// Signed webhook delivery is only a wakeup hint. The worker confirms ownership and
// final state by GET; replayed or out-of-order events cannot change a code's state.
func notifyAutomationWebhook(payload map[string]interface{}) {
	request, ok := payload["client_request_id"].(string)
	if !ok || request == "" {
		return
	}
	orderID := anyToInt64(payload["order_id"])
	if orderID <= 0 {
		return
	}
	_, _ = db.DB.Exec("UPDATE automation_watch SET next_check=MIN(next_check,?) WHERE local_id IN (SELECT id FROM local_cdks WHERE request_id=? AND upstream_id=?)", time.Now().Unix()+30, request, orderID)
}
