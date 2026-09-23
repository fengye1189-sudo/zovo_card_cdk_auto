package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func startPreparedPro5x(c *gin.Context, r localCode, token, pf string, credential json.RawMessage, settings localSettings, policy automationPolicy, policyVersion, fee int64) {
	name := proDedicatedName(r.Plan)
	if !policy.Sync || !policy.Open || policy.Paused || policy.DailyBudget <= 0 || fee < 0 || fee > 50 {
		localError(c, 409, name+" 自动处理条件尚未满足，未提交升级")
		return
	}
	reserve, err := loadPro5xReserve()
	if err != nil || reserve.State != "ready" || reserve.CardID <= 0 {
		localError(c, 409, name+" 预备卡正在补充或核对，请稍后再试")
		return
	}
	cli := cardplatform.NewFromSettings()
	version, currentFee, err := cli.DirectPricing(c.Request.Context(), r.Plan)
	if err != nil || version != r.Version || currentFee != fee || currentFee > 50 {
		localError(c, 409, name+" 套餐或服务费发生变化，未提交升级")
		return
	}
	candidates, err := cli.DirectCandidatesForPlan(c.Request.Context(), r.Plan)
	usable := false
	if err == nil {
		for _, candidate := range candidates {
			if candidate.CardID == reserve.CardID && candidate.Usable(pro5xInitialMinor) {
				usable = true
				break
			}
		}
	}
	if !usable {
		localError(c, 409, name+" 预备卡当前未通过支付可用性核查，请稍后再试")
		return
	}
	spendable, err := cli.AutomationSpendable(c.Request.Context())
	wallet, ok := usdMinor(spendable)
	if err != nil || !ok || wallet-fee < policy.WalletFloor {
		localError(c, 409, "平台可消费余额不足或低于保留额，未提交升级")
		return
	}
	requestID, err := localRandom()
	if err != nil {
		localError(c, 503, "暂时无法创建升级订单")
		return
	}
	requestID = "pro5x-pay-" + requestID
	scopeConfig := cardplatform.LoadConfig()
	scope := localHash(scopeConfig.SiteBase + "|" + scopeConfig.APIKey)
	dailyBudget := policy.DailyBudget
	if dailyBudget > proDailyMaximum {
		dailyBudget = proDailyMaximum
	}
	now := time.Now().Unix()
	tx, err := db.DB.Begin()
	if err != nil {
		localError(c, 503, "升级订单暂时无法锁定")
		return
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE local_cdks SET status='reserved',message='订单处理中，请勿重复提交'
		WHERE id=? AND plan='pro_5x' AND status='unused' AND token_hash=? AND preflight_hash=?
		AND credential_hash=? AND preflight_expires>? AND expires_at>?
		AND EXISTS(SELECT 1 FROM operations_products WHERE id=COALESCE((SELECT product_id FROM operations_product_bindings WHERE local_id=?),'plus') AND plan='pro_5x' AND enabled=1)
		AND EXISTS(SELECT 1 FROM automation_policy WHERE id=1 AND version=?)`,
		r.ID, localHash(token), localHash(pf), credentialBinding(credential, settings), now, now, r.ID, policyVersion)
	if err != nil {
		localError(c, 503, "升级订单锁定失败，未提交升级")
		return
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		localError(c, 409, "订单或设置已变化，请查询结果")
		return
	}
	cardID, err := claimPro5xReserve(tx, r.ID, requestID, scope, fee, dailyBudget, now)
	if err != nil {
		localError(c, 409, "Pro 5X 预备卡已被使用、每日预算不足或状态已变化，请稍后再试")
		return
	}
	if _, err = tx.Exec("UPDATE local_cdks SET card_id=? WHERE id=?", cardID, r.ID); err != nil || tx.Commit() != nil {
		localError(c, 503, "订单确认结果待核对，请勿重复提交")
		return
	}
	secretCopy := append(json.RawMessage(nil), credential...)
	go runPreparedPro5x(r, cardID, requestID, pf, secretCopy, settings, policy, policyVersion, scope, fee)
	r, _ = loadLocal("id", strconv.FormatInt(r.ID, 10))
	c.JSON(202, localPublicResult(r))
}

func runPreparedPro5x(r localCode, cardID int64, requestID, pf string, credential json.RawMessage, settings localSettings, policy automationPolicy, policyVersion int64, scope string, fee int64) {
	defer func() {
		for i := range credential {
			credential[i] = 0
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli := cardplatform.NewFromSettings()
	fail := func(adminMessage string) {
		_, _ = db.DB.Exec("UPDATE pro_dedicated_orders SET state='review' WHERE local_id=?", r.ID)
		_, _ = db.DB.Exec("UPDATE local_cdks SET status='review',message='订单需要商家核对，请勿重复提交' WHERE id=?", r.ID)
		autoAlert(fmt.Sprintf("pro:%d", r.ID), r.ID, adminMessage)
	}
	valid := func() bool {
		current, version, err := readAutomationPolicy()
		cfg := cardplatform.LoadConfig()
		currentSettings := settingsForLocalPlan(readLocalSettings(), r.Plan)
		return err == nil && version == policyVersion && current.Sync && current.Open && !current.Paused &&
			currentSettings.Enabled && currentSettings.ProDedicatedEnabled &&
			credentialBinding(credential, currentSettings) == r.Cred &&
			localHash(cfg.SiteBase+"|"+cfg.APIKey) == scope && r.PFExpires > time.Now().Unix() && operationsProductAvailable(r.ID)
	}
	if !valid() {
		fail("Pro 5X 预备卡已领取，但设置或预检发生变化；未付款，请人工核对该卡。")
		return
	}
	version, currentFee, err := cli.DirectPricing(ctx, r.Plan)
	if err != nil || version != r.Version || currentFee != fee || currentFee > 50 {
		fail("Pro 5X 预备卡已领取，但套餐或服务费变化；未付款，请人工核对该卡。")
		return
	}
	candidates, err := cli.DirectCandidatesForPlan(ctx, r.Plan)
	usable := false
	if err == nil {
		for _, candidate := range candidates {
			if candidate.CardID == cardID && candidate.Usable(pro5xInitialMinor) {
				usable = true
				break
			}
		}
	}
	if !usable || !valid() {
		fail("Pro 5X 预备卡已领取，但付款可用性或预检核查未通过；未付款，请人工核对该卡。")
		return
	}
	tx, err := db.DB.Begin()
	if err != nil {
		fail("Pro 5X 付款暂时无法锁定；未付款。")
		return
	}
	result, err := tx.Exec(`UPDATE pro_dedicated_orders SET state='submitted'
		WHERE local_id=? AND card_id=? AND money_id=? AND state='funding'`, r.ID, cardID, requestID)
	if err != nil {
		tx.Rollback()
		fail("Pro 5X 付款状态待核对；未重复付款。")
		return
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		tx.Rollback()
		fail("Pro 5X 订单状态已变化；未重复付款。")
		return
	}
	result, err = tx.Exec(`UPDATE local_cdks SET request_id=?,message='订单处理中，请勿重复提交'
		WHERE id=? AND status='reserved' AND request_id='' AND preflight_expires>?
		AND EXISTS(SELECT 1 FROM automation_policy WHERE id=1 AND version=?)
		AND NOT EXISTS(SELECT 1 FROM local_cdks WHERE id<>? AND card_id=? AND status IN ('reserved','review'))`,
		requestID, r.ID, time.Now().Unix(), policyVersion, r.ID, cardID)
	if err != nil {
		tx.Rollback()
		fail("Pro 5X 付款锁定失败；未付款。")
		return
	}
	changed, _ = result.RowsAffected()
	if changed != 1 {
		_ = tx.Rollback()
		fail("Pro 5X 付款锁定结果待核对；不会重复付款。")
		return
	}
	if err = tx.Commit(); err != nil {
		fail("Pro 5X 付款锁定结果待核对；不会重复付款。")
		return
	}
	raw, err := cli.DirectOrder(ctx, gin.H{
		"product": "gpt", "no_auto_card_switch": true, "card_id": cardID,
		"plan": r.Plan, "credential": credential, "preflight_token": pf,
		"client_request_id": requestID, "pricing_version": r.Version,
	}, requestID)
	var order struct {
		ID int64 `json:"id"`
	}
	if err != nil || json.Unmarshal(raw, &order) != nil || order.ID <= 0 {
		fail("Pro 5X 付款结果待核对；该卡不会用于其他订单，系统不会再次付款。")
		return
	}
	_, _ = db.DB.Exec("UPDATE local_cdks SET upstream_id=?,message='订单处理中，请勿重复提交' WHERE id=? AND request_id=?", order.ID, r.ID, requestID)
	_ = policy
}
