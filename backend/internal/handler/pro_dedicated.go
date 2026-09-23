package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
	"time"
)

const proInitialMinor int64 = 15000
const pro5xInitialMinor int64 = 10000
// proDailyMaximum is the owner-authorized shared rolling 24-hour funds cap.
// Keep this safety ceiling aligned with automation_policy.daily_budget_minor.
const proDailyMaximum int64 = 200000

func proDedicatedInitialMinor(plan string) int64 {
	if plan == "pro_5x" {
		return pro5xInitialMinor
	}
	return proInitialMinor
}

func proDedicatedName(plan string) string {
	if plan == "pro_5x" {
		return "Pro 5X"
	}
	return "Pro 20X"
}

func startProDedicated(c *gin.Context, r localCode, token, pf string, credential json.RawMessage, s localSettings, p automationPolicy, policyVersion, fee int64) {
	if r.Plan == "pro_5x" {
		startPreparedPro5x(c, r, token, pf, credential, s, p, policyVersion, fee)
		return
	}
	cli := cardplatform.NewFromSettings()
	initial, name := proDedicatedInitialMinor(r.Plan), proDedicatedName(r.Plan)
	if !p.Sync || !p.Open || p.Paused || p.First == "" || p.Last == "" || p.DailyBudget <= 0 || p.DailyOpen <= 0 || fee < 0 || fee > 50 {
		localError(c, 409, name+" 专卡自动开卡条件尚未满足，未开卡或付款")
		return
	}
	products, e := cli.AutomationProducts(c.Request.Context())
	if e != nil {
		localError(c, 502, "暂时无法核查专卡费用，未开卡")
		return
	}
	var cost int64
	matches := 0
	for _, product := range products {
		if product.Code == "P5378OX" {
			matches++
			var ok bool
			cost, ok = productCost(product, initial, true)
			if !ok || product.RechargeFee == nil || *product.RechargeFee != 0 || cost != initial+100 {
				localError(c, 409, "专卡费用或限制发生变化，未开卡；请联系管理员核对")
				return
			}
		}
	}
	if matches != 1 {
		localError(c, 409, "指定专卡产品不可用，未开卡")
		return
	}
	// Reserve API fees as well, keeping the total shared rolling budget bounded.
	cost += fee
	spendable, e := cli.AutomationSpendable(c.Request.Context())
	wallet, ok := usdMinor(spendable)
	if e != nil || !ok || wallet-cost < p.WalletFloor {
		localError(c, 409, "平台可用余额不足或低于保留额，未开卡")
		return
	}
	request, e := localRandom()
	if e != nil {
		localError(c, 503, "暂时无法创建专卡订单")
		return
	}
	request = "pro-" + request
	scopeCfg := cardplatform.LoadConfig()
	scope := localHash(scopeCfg.SiteBase + "|" + scopeCfg.APIKey)
	daily := p.DailyBudget
	if daily > proDailyMaximum {
		daily = proDailyMaximum
	}
	tx, e := db.DB.Begin()
	if e != nil {
		localError(c, 503, "专卡订单暂时无法锁定")
		return
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	result, e := tx.Exec(`UPDATE local_cdks SET status='reserved',message=? WHERE id=? AND plan=? AND status='unused' AND token_hash=? AND preflight_hash=? AND credential_hash=? AND preflight_expires>? AND expires_at>?
 AND EXISTS(SELECT 1 FROM operations_products WHERE id=COALESCE((SELECT product_id FROM operations_product_bindings WHERE local_id=?),'plus') AND plan=? AND enabled=1)
 AND EXISTS(SELECT 1 FROM automation_policy WHERE id=1 AND version=?)`, "正在准备 "+name+" 专用卡，请勿重复提交", r.ID, r.Plan, localHash(token), localHash(pf), credentialBinding(credential, s), now, now, r.ID, r.Plan, policyVersion)
	if e != nil {
		localError(c, 503, "专卡订单锁定失败，未开卡")
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		localError(c, 409, "订单或设置已变化，请查询结果")
		return
	}
	result, e = tx.Exec(`INSERT INTO automation_money(id,action,card_id,amount_minor,reserved_minor,before_minor,scope,state,created_at)
 SELECT ?,'pro_open',0,?,?,0,?,'inflight',?
 WHERE COALESCE((SELECT SUM(reserved_minor) FROM automation_money WHERE created_at>?),0)+?<=?
 AND NOT EXISTS(SELECT 1 FROM automation_money WHERE state IN ('inflight','unknown','pending'))
 AND (SELECT COUNT(*) FROM automation_money WHERE action IN ('open','pro_open') AND created_at>?)<?`, request, initial, cost, scope, now, now-86400, cost, daily, now-86400, p.DailyOpen)
	if e != nil {
		localError(c, 503, "专卡预算预留失败，未开卡")
		return
	}
	n, _ = result.RowsAffected()
	if n != 1 {
		localError(c, 409, "每日共用预算或开卡数量不足，或有待核对资金操作；未开卡")
		return
	}
	if _, e = tx.Exec("INSERT INTO pro_dedicated_orders(local_id,money_id,state,created_at,api_fee_minor) VALUES(?,?,'opening',?,?)", r.ID, request, now, fee); e != nil {
		localError(c, 409, "此订单已有专用卡申请，请查询结果")
		return
	}
	if e = tx.Commit(); e != nil {
		localError(c, 503, "订单结果待核对，请勿重复操作")
		return
	}
	// Credentials stay in this worker's memory only. No session/password is saved
	// to the database, logs or job payloads. A restart stops for manual review.
	secretCopy := append(json.RawMessage(nil), credential...)
	go runProDedicated(r, request, pf, secretCopy, s, p, policyVersion, scope, fee)
	r, _ = loadLocal("id", fmt.Sprintf("%d", r.ID))
	c.JSON(202, localPublicResult(r))
}

func runProDedicated(r localCode, request, pf string, credential json.RawMessage, s localSettings, p automationPolicy, policyVersion int64, scope string, fee int64) {
	initial, name := proDedicatedInitialMinor(r.Plan), proDedicatedName(r.Plan)
	defer func() {
		for i := range credential {
			credential[i] = 0
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cli := cardplatform.NewFromSettings()
	opened := false
	balanceVerified := false
	fail := func(message string) {
		state := "cancelled"
		if opened && !balanceVerified {
			state = "unknown"
		}
		if balanceVerified {
			state = "balance_verified"
		}
		_, _ = db.DB.Exec("UPDATE automation_money SET state=? WHERE id=?", state, request)
		_, _ = db.DB.Exec("UPDATE pro_dedicated_orders SET state='review' WHERE local_id=?", r.ID)
		_, _ = db.DB.Exec("UPDATE local_cdks SET status='review',message=? WHERE id=?", message, r.ID)
		autoAlert(fmt.Sprintf("pro:%d", r.ID), r.ID, message)
	}
	valid := func() bool {
		current, v, e := readAutomationPolicy()
		cfg := cardplatform.LoadConfig()
		return e == nil && v == policyVersion && !current.Paused && current.Open && current.Sync && settingsForLocalPlan(readLocalSettings(), r.Plan).Enabled && credentialBinding(credential, settingsForLocalPlan(readLocalSettings(), r.Plan)) == r.Cred && localHash(cfg.SiteBase+"|"+cfg.APIKey) == scope && r.PFExpires > time.Now().Unix() && operationsProductAvailable(r.ID)
	}
	if !valid() {
		fail("专卡设置或预检已变化，未发起开卡，请联系管理员")
		return
	}
	opened = true // An ambiguous response must never cause a second opening.
	card, e := cli.AutomationOpen(ctx, "P5378OX", p.First, p.Last, initial, request)
	if e != nil {
		fail("专卡开卡结果不明，请联系管理员核对；不会重复开卡")
		return
	}
	tx, e := db.DB.Begin()
	if e != nil {
		fail("专卡记录待核对；不会重复开卡")
		return
	}
	if _, e = tx.Exec("UPDATE pro_dedicated_orders SET card_id=?,state='funding' WHERE local_id=?", card, r.ID); e != nil {
		tx.Rollback()
		fail("专卡绑定异常，请管理员核对")
		return
	}
	if _, e = tx.Exec("UPDATE automation_money SET result_card_id=?,state='pending' WHERE id=?", card, request); e != nil {
		tx.Rollback()
		fail("专卡资金记录待核对")
		return
	}
	if _, e = tx.Exec("UPDATE local_cdks SET card_id=?,message=? WHERE id=?", card, fmt.Sprintf("专用卡已申请，正在核查 $%.2f 充值到账", float64(initial)/100), r.ID); e != nil {
		tx.Rollback()
		fail("专卡订单记录待核对")
		return
	}
	if e = tx.Commit(); e != nil {
		fail("专卡记录提交结果待核对")
		return
	}
	for {
		inventory, e := automationInventory(ctx, cli)
		if e == nil {
			for _, item := range inventory {
				amount, ok := usdMinor(item.Balance)
				if item.ID == card && item.Status == "ACTIVE" && ok && amount >= initial {
					balanceVerified = true
					break
				}
			}
		}
		if balanceVerified {
			break
		}
		if !valid() {
			fail("专卡充值尚未确认或设置已变化，请管理员核对；不会再次充值")
			return
		}
		select {
		case <-ctx.Done():
			fail("专卡充值到账查询超时，请管理员核对；不会重复开卡或补款")
			return
		case <-time.After(10 * time.Second):
		}
	}
	if _, e = db.DB.Exec("UPDATE automation_money SET state='balance_verified' WHERE id=?", request); e != nil {
		fail("专卡余额已到账，资金记录待核对；尚未付款")
		return
	}
	if !valid() {
		fail("专卡已准备，但预检或设置已变化；尚未付款，请联系管理员")
		return
	}
	version, currentFee, e := cli.DirectPricing(ctx, r.Plan)
	if e != nil || version != r.Version || currentFee != fee || currentFee > 50 {
		fail("专卡已准备，但套餐或服务费变化；尚未付款")
		return
	}
	candidates, e := cli.DirectCandidatesForPlan(ctx, r.Plan)
	usable := false
	if e == nil {
		for _, candidate := range candidates {
			if candidate.CardID == card && candidate.Usable(initial) {
				usable = true
			}
		}
	}
	if !usable {
		fail("专卡已准备，但卡台未确认可支付 " + name + "；尚未付款，请管理员核对")
		return
	}
	if !valid() {
		fail("专卡已准备，但预检已失效；尚未付款")
		return
	}
	payID := request + "-pay"
	tx, e = db.DB.Begin()
	if e != nil {
		fail("专卡付款暂时无法锁定；尚未付款")
		return
	}
	result, e := tx.Exec("UPDATE pro_dedicated_orders SET state='submitted' WHERE local_id=? AND card_id=? AND state='funding'", r.ID, card)
	if e != nil {
		tx.Rollback()
		fail("专卡付款状态待核对")
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		tx.Rollback()
		fail("专卡订单状态已变化")
		return
	}
	result, e = tx.Exec("UPDATE local_cdks SET request_id=?,message='专卡余额已确认，正在提交升级' WHERE id=? AND status='reserved' AND request_id='' AND preflight_expires>? AND EXISTS(SELECT 1 FROM automation_policy WHERE id=1 AND version=?) AND NOT EXISTS(SELECT 1 FROM local_cdks WHERE id<>? AND card_id=?)", payID, r.ID, time.Now().Unix(), policyVersion, r.ID, card)
	if e != nil {
		tx.Rollback()
		fail("专卡付款锁定失败；尚未付款")
		return
	}
	n, _ = result.RowsAffected()
	if n != 1 {
		tx.Rollback()
		fail("专卡或预检状态变化，未重复付款")
		return
	}
	if e = tx.Commit(); e != nil {
		fail("专卡付款锁定结果待核对")
		return
	}
	raw, e := cli.DirectOrder(ctx, gin.H{"product": "gpt", "no_auto_card_switch": true, "card_id": card, "plan": r.Plan, "credential": credential, "preflight_token": pf, "client_request_id": payID, "pricing_version": r.Version}, payID)
	var order struct {
		ID int64 `json:"id"`
	}
	if e != nil || json.Unmarshal(raw, &order) != nil || order.ID <= 0 {
		fail(name + " 付款结果待核对；专卡不再复用，不会再次付款")
		return
	}
	_, _ = db.DB.Exec("UPDATE local_cdks SET upstream_id=?,message=? WHERE id=? AND request_id=?", order.ID, name+" 升级已受理，正在确认结果", r.ID, payID)
}

func recoverInterruptedProOrders() {
	_, _ = db.DB.Exec("UPDATE automation_money SET state='unknown' WHERE id IN (SELECT money_id FROM pro_dedicated_orders WHERE state IN ('opening','funding')) AND state IN ('inflight','pending')")
	_, _ = db.DB.Exec("UPDATE local_cdks SET status='review',message='服务重启，专卡准备结果需管理员核对；不会重复开卡或付款' WHERE id IN (SELECT local_id FROM pro_dedicated_orders WHERE state IN ('opening','funding'))")
	result, err := db.DB.Exec("UPDATE pro_dedicated_orders SET state='review' WHERE state IN ('opening','funding')")
	if err == nil {
		if n, _ := result.RowsAffected(); n > 0 {
			autoAlert("pro_restart", 0, "服务重启前有 Pro 专卡准备任务未完成，请核对卡片与资金；系统不会重复开卡或付款。")
		}
	}
}
