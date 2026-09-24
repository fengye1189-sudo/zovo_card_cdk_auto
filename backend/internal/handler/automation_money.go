package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func usdMinor(v *float64) (int64, bool) {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 || *v > 1000000 {
		return 0, false
	}
	return int64(math.Floor(*v*100 + 0.000001)), true
}
func productCost(p cardplatform.AutomationProduct, amount int64, open bool) (int64, bool) {
	min, a := usdMinor(p.Min)
	max, b := usdMinor(p.Max)
	if a {
		min = int64(math.Ceil(*p.Min*100 - 0.000001))
	}
	// Owner confirmed that all existing-card recharges start at USD 10.
	// The shared product minimum is not the recharge minimum. Keep it for
	// opening only; all other product/funding guards remain enforced.
	if !open {
		min = 1000
		a = true
	}
	if !a || !b || amount < min || amount > max || p.Restricted == nil {
		return 0, false
	}
	for _, s := range *p.Restricted {
		u := strings.ToUpper(s)
		if strings.Contains(u, "CHATGPT") || strings.Contains(u, "OPENAI") {
			return 0, false
		}
	}
	if open {
		_, ok := usdMinor(p.OpenFee)
		if !ok {
			return 0, false
		}
		fee := int64(math.Ceil(*p.OpenFee*100 - 0.000001))
		return amount + fee, true
	}
	if p.RechargeFee == nil || math.IsNaN(*p.RechargeFee) || math.IsInf(*p.RechargeFee, 0) || *p.RechargeFee < 0 || *p.RechargeFee > 1 {
		return 0, false
	}
	return amount + int64(math.Ceil(float64(amount)*(*p.RechargeFee))), true
}

const legacyInventoryAlert = "无法完整核查卡余额，已跳过自动补款与开卡。卡片超过 100 张时需要调整扫描方案。"
const automationReadyCardFloor = 2

func shouldOpenAutomationCard(ready int) bool {
	return ready <= automationReadyCardFloor
}

type inventoryScanError struct{ reason string }

func (e *inventoryScanError) Error() string { return e.reason }

func inventoryAlertMessage(err error) string {
	reason := "卡台返回的卡片数据格式异常"
	var scan *inventoryScanError
	var api *cardplatform.APIError
	var network net.Error
	switch {
	case errors.As(err, &scan):
		reason = scan.reason
	case errors.Is(err, context.DeadlineExceeded):
		reason = "卡片余额查询超时"
	case errors.Is(err, context.Canceled):
		reason = "卡片余额查询被中断"
	case errors.As(err, &api):
		reason = fmt.Sprintf("卡台查询接口失败（HTTP %d，业务码 %d）", api.HTTPStatus, api.Code)
	case errors.As(err, &network):
		if network.Timeout() {
			reason = "卡片余额查询超时"
		} else {
			reason = "无法连接卡台查询接口"
		}
	}
	// Never persist raw upstream bodies, URLs or credentials in user-facing alerts.
	return reason + "，本轮已跳过自动补款与开卡；完整查询恢复后将自动解除此提醒。"
}

func recordInventoryScan(err error) {
	if err != nil {
		autoAlert("money_inventory", 0, inventoryAlertMessage(err))
	} else {
		autoResolve("money_inventory")
	}
	// Migrate only this exact historical warning; preserve other money alerts.
	_, _ = db.DB.Exec("UPDATE automation_alerts SET resolved=1 WHERE alert_key='money' AND message=?", legacyInventoryAlert)
}

func automationInventory(ctx context.Context, cli *cardplatform.Client) ([]cardplatform.CardChoice, error) {
	cards := []cardplatform.CardChoice{}
	seen := map[int64]bool{}
	for page := 1; page <= 5; page++ {
		list, total, e := cli.CardChoicesSync(ctx, page)
		if e != nil {
			return nil, e
		}
		if total > 100 {
			return nil, &inventoryScanError{fmt.Sprintf("卡台返回 %d 张卡，超过当前 100 张扫描上限", total)}
		}
		for _, c := range list {
			if seen[c.ID] {
				return nil, &inventoryScanError{"分页查询返回重复卡片，无法确认列表完整"}
			}
			seen[c.ID] = true
			cards = append(cards, c)
		}
		if page*20 >= total {
			if len(cards) != total {
				return nil, &inventoryScanError{fmt.Sprintf("卡片列表不完整（实际读取 %d 张，卡台报告 %d 张）", len(cards), total)}
			}
			return cards, nil
		}
	}
	return nil, &inventoryScanError{"卡片分页扫描未完成"}
}
func maintainAutomationCards(ctx context.Context) {
	now := time.Now().Unix()
	lock, e := db.DB.Exec("UPDATE automation_runtime SET money_after=? WHERE id=1 AND money_after<=?", now+300, now)
	if e != nil {
		return
	}
	n, _ := lock.RowsAffected()
	if n != 1 {
		return
	}
	p, version, e := readAutomationPolicy()
	if e != nil || !p.Sync {
		return
	}
	var uncertain int
	_ = db.DB.QueryRow("SELECT COUNT(*) FROM automation_money WHERE state IN ('inflight','unknown')").Scan(&uncertain)
	var pending int
	_ = db.DB.QueryRow("SELECT COUNT(*) FROM automation_money WHERE state='pending'").Scan(&pending)
	if pending == 0 && uncertain == 0 && (p.Paused || (!p.Topup && !p.Open && !p.Retire) || (!readLocalSettings().Enabled && !p.Retire)) {
		return
	}
	scopeConfig := cardplatform.LoadConfig()
	scope := localHash(scopeConfig.SiteBase + "|" + scopeConfig.APIKey)
	cli := cardplatform.New(scopeConfig)
	inventory, e := automationInventory(ctx, cli)
	recordInventoryScan(e)
	if e != nil {
		return
	}
	verifyMoneyOperationsForScope(inventory, p, scope)
	if uncertain > 0 {
		var remaining int
		_ = db.DB.QueryRow("SELECT COUNT(*) FROM automation_money WHERE state IN ('inflight','unknown')").Scan(&remaining)
		if remaining > 0 {
			autoAlert("money", 0, "存在结果未确认的资金操作，已阻止新的资金操作与充值；请先在 Zovo 核对，本站不会自动重试。")
			return
		}
	}
	reconcilePro5xReserve(inventory)
	reconcileCreatedCardEnrollment(inventory, p)
	// A pending provider-accepted money operation is only reconciled in this
	// cycle. Even when it is confirmed, defer any new funding decision until the
	// next cycle so one delayed observation cannot trigger back-to-back spending.
	if pending > 0 || uncertain > 0 {
		return
	}
	if automationBlocked() {
		return
	}
	products, e := cli.AutomationProducts(ctx)
	if e != nil {
		autoAlert("money", 0, "无法核查产品费率，未进行资金操作。")
		return
	}
	productMap := map[string]cardplatform.AutomationProduct{}
	for _, product := range products {
		if _, exists := productMap[product.Code]; exists {
			autoAlert("money", 0, "产品资料重复，已停止自动资金操作。")
			return
		}
		productMap[product.Code] = product
	}
	if e = catalogAutomationInventory(inventory, productMap, now); e != nil {
		autoAlert("card_lifecycle", 0, "卡片周期或卡头统计更新失败，本轮未执行销卡、补款或开卡。")
		return
	}
	reconcileRetiredCards(inventory, now)
	if p.Retire && processQueuedCardRetirement(ctx, cli, inventory, scope, version, now) {
		return
	}
	if maintainPro5xReserve(ctx, cli, inventory, p, version, scope, productMap, now) {
		return
	}
	if (!p.Topup && !p.Open) || !readLocalSettings().Enabled {
		return
	}
	localRaw, e := db.GetSetting("local_cdk_settings")
	var cfg localSettings
	if e != nil || json.Unmarshal([]byte(localRaw), &cfg) != nil || !cfg.Enabled {
		return
	}
	if cfg.MinCardBalanceMinor <= 0 || (p.Topup && p.Target < cfg.MinCardBalanceMinor) || (p.Open && p.InitAmount < cfg.MinCardBalanceMinor) {
		autoAlert("money", 0, "自动补款目标或开卡金额低于支付卡余额门槛，未执行资金操作。")
		return
	}
	var fee int64
	pricingErr := retryAutomationRead(ctx, func(readCtx context.Context) error {
		var err error
		_, fee, err = cli.DirectPricing(readCtx, "plus")
		return err
	})
	if pricingErr != nil {
		message := "Plus 费率与通道" + automationReadReason(pricingErr) + "，本轮未执行资金操作；查询恢复后自动解除。"
		if errors.Is(pricingErr, cardplatform.ErrDirectPlanUnavailable) {
			message = "上游明确返回 Plus 通道不可购买或已停用，本轮未执行资金操作；通道恢复后自动解除。"
		}
		autoAlert("money_pricing", 0, message)
		return
	}
	autoResolve("money_pricing")
	if fee > cfg.MaxFeeMinor {
		autoAlert("money_fee", 0, fmt.Sprintf("Plus 服务费 $%.2f 超过本站上限 $%.2f，未执行资金操作；请核对费率，不会自动提高上限。", float64(fee)/100, float64(cfg.MaxFeeMinor)/100))
		return
	}
	autoResolve("money_fee")
	resolveLegacyMoneyAlert("Plus 通道或服务费不符合本站充值规则，未执行资金操作。")
	ids := localCardIDs(cfg)
	selected := map[int64]bool{}
	for _, id := range ids {
		selected[id] = true
	}
	// Grandfathered 5X cards keep their prior behavior. Cards governed by the
	// three-use policy are added only after their third completed 5X upgrade.
	rows, selectErr := db.DB.Query(`SELECT p.card_id
		FROM pro_dedicated_orders p JOIN local_cdks c ON c.id=p.local_id
		WHERE p.card_id>0 AND p.state='completed' AND c.plan='pro_5x' AND c.status='consumed'`)
	if selectErr != nil {
		return
	}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			selected[id] = true
		}
	}
	if rows.Err() != nil {
		rows.Close()
		return
	}
	rows.Close()
	rows, selectErr = db.DB.Query("SELECT card_id FROM pro5x_card_policy WHERE completed_uses>=?", pro5xUsesBeforePlusPool)
	if selectErr != nil {
		return
	}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			selected[id] = true
		}
	}
	if rows.Err() != nil {
		rows.Close()
		return
	}
	rows.Close()
	var candidates []cardplatform.DirectCandidate
	e = retryAutomationRead(ctx, func(readCtx context.Context) error {
		var err error
		candidates, err = cli.DirectCandidates(readCtx)
		return err
	})
	if e != nil {
		autoAlert("money_candidates", 0, "支付卡可用性"+automationReadReason(e)+"，本轮未执行资金操作；查询恢复后自动解除。")
		return
	}
	autoResolve("money_candidates")
	resolveLegacyMoneyAlert("无法核查卡片是否可用于充值，未进行资金操作。")
	eligible := map[int64]bool{}
	// Count distinct usable cards, not remaining payment attempts on one card.
	ready := map[int64]bool{}
	for _, candidate := range candidates {
		eligible[candidate.CardID] = candidate.Skip != nil && !*candidate.Skip && candidate.SkipReason == "" && candidate.LightRemain != nil && (*candidate.LightRemain > 0 || *candidate.LightRemain == -1)
		// Payment reserve count includes the authorized existing card, while
		// funding eligibility above retains the unmodified upstream exclusion.
		candidate = existingPaymentCandidate(candidate, inventory, "plus")
		if selected[candidate.CardID] && candidate.Usable(cfg.MinCardBalanceMinor) {
			capacity, capacityErr := localCardHasPaymentCapacity(candidate.CardID, 0)
			if capacityErr != nil {
				return
			}
			if capacity {
				ready[candidate.CardID] = true
			}
		}
	}
	action := ""
	openProduct := p.Product
	openBIN, selectionMode := "", ""
	var cardID, amount, before, cost int64
	if p.Topup {
		for _, card := range inventory {
			var dedicated int
			if err := db.DB.QueryRow(`SELECT COUNT(*)
				FROM pro_dedicated_orders p JOIN local_cdks c ON c.id=p.local_id
				WHERE p.card_id=? AND NOT (p.state='completed' AND c.plan='pro_5x' AND c.status='consumed')`, card.ID).Scan(&dedicated); err != nil {
				return
			}
			var heldFor5x int
			if err := db.DB.QueryRow("SELECT COUNT(*) FROM pro5x_card_policy WHERE card_id=? AND completed_uses<?", card.ID, pro5xUsesBeforePlusPool).Scan(&heldFor5x); err != nil {
				return
			}
			dedicated += heldFor5x
			if dedicated > 0 {
				continue
			}
			if !selected[card.ID] || card.Status != "ACTIVE" || !eligible[card.ID] {
				continue
			}
			capacity, capacityErr := localCardHasPaymentCapacity(card.ID, 0)
			if capacityErr != nil || !capacity {
				continue
			}
			balance, ok := usdMinor(card.Balance)
			if !ok || balance >= p.Threshold {
				continue
			}
			var busy int
			err := db.DB.QueryRow("SELECT COUNT(*) FROM local_cdks WHERE card_id=? AND status IN ('reserved','review')", card.ID).Scan(&busy)
			if err != nil || busy > 0 {
				continue
			}
			// A verified earlier topup does not block replenishment after another
			// completed payment. Pending/unknown funding, busy cards and the
			// rolling daily budget are still guarded before reservation.
			amount = p.Target - balance
			// Respect the recharge minimum even when only a small deficit remains.
			if amount < 1000 {
				amount = 1000
			}
			product, ok := productMap[card.Product]
			if !ok {
				autoAlert(fmt.Sprintf("topup:%d", card.ID), 0, fmt.Sprintf("卡片 #%d 余额低于补款阈值，但产品费率未返回，已跳过补款。", card.ID))
				continue
			}
			cost, ok = productCost(product, amount, false)
			if !ok || balance+amount > p.CardCeiling {
				autoAlert(fmt.Sprintf("topup:%d", card.ID), 0, fmt.Sprintf("卡片 #%d 余额低于补款阈值，但本次补款 $%.2f 不符合产品金额、商户限制或补款后卡内上限，已跳过补款，请核对规则。", card.ID, float64(amount)/100))
				continue
			}
			autoResolve(fmt.Sprintf("topup:%d", card.ID))
			action, cardID, before = "topup", card.ID, balance
			openProduct = card.Product
			break
		}
	}
	if action == "" && p.Open && shouldOpenAutomationCard(len(ready)) {
		// An opened but not enrolled card must not cause an endless sequence of new cards.
		rows, e := db.DB.Query("SELECT result_card_id FROM automation_money WHERE action='open' AND result_card_id>0 AND scope=?", scope)
		if e != nil {
			return
		}
		unselectedIDs := []int64{}
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil && !selected[id] {
				unselectedIDs = append(unselectedIDs, id)
			}
		}
		rows.Close()
		var exhausted map[int64]bool
		if cfg.UseUpstreamCardLimit && len(unselectedIDs) > 0 {
			exhausted, e = upstreamExhaustedCards(ctx, cli)
			if e != nil {
				return
			}
		}
		for _, id := range unselectedIDs {
			capacity, err := localCardHasPaymentCapacity(id, 0)
			if err != nil {
				return
			}
			if capacity && !exhausted[id] {
				autoAlert("money", 0, "已有自动开出的卡尚未加入支付名单，请先在卡密管理中勾选；不会继续开新卡。")
				return
			}
		}
		var product cardplatform.AutomationProduct
		var ok bool
		if p.AutoProduct {
			product, selectionMode, e = selectAutomationProduct(products, p.InitAmount, now)
			if e != nil {
				autoAlert("money", 0, "当前没有符合开卡金额和商户限制的可用卡头，未开卡。")
				return
			}
			openProduct = product.Code
			ok = true
		} else {
			product, ok = productMap[p.Product]
			if !ok {
				autoAlert("money", 0, "指定自动开卡产品未在上游产品列表中返回，未开卡；请核对产品码。")
				return
			}
			selectionMode = "fixed"
		}
		openBIN = product.Bin
		amount = p.InitAmount
		cost, ok = productCost(product, amount, true)
		if !ok || amount > p.CardCeiling {
			autoAlert("money", 0, "指定开卡产品的金额或商户限制不符合规则，未开卡。")
			return
		}
		action = "open"
	}
	if action == "" {
		return
	}
	spendable, e := cli.AutomationSpendable(ctx)
	wallet, ok := usdMinor(spendable)
	if e != nil || !ok || wallet-cost < p.WalletFloor {
		autoAlert("money", 0, "Zovo 可消费余额不足或未达到钱包保留额，未进行自动资金操作。")
		return
	}
	current, currentVersion, e := readAutomationPolicy()
	if e != nil || currentVersion != version || current.Paused || !readLocalSettings().Enabled {
		return
	}
	id, e := localRandom()
	if e != nil {
		return
	}
	id = "auto-" + id
	// Atomic reservation caps all automatic card funding/opening over a rolling 24h.
	// Pending, unknown and refunded/failed attempts retain their reserved budget.
	currentConfig := cardplatform.LoadConfig()
	if localHash(currentConfig.SiteBase+"|"+currentConfig.APIKey) != scope {
		return
	}
	tx, e := db.DB.Begin()
	if e != nil {
		autoAlert("money", 0, "无法锁定自动资金操作，未进行操作。")
		return
	}
	defer tx.Rollback()
	r, e := tx.Exec(`INSERT INTO automation_money(id,action,card_id,amount_minor,reserved_minor,before_minor,scope,state,created_at)
 SELECT ?,?,?,?,?,?,?,'inflight',? WHERE ?<=? AND ?>0
 AND COALESCE((SELECT SUM(reserved_minor) FROM automation_money WHERE created_at>?),0)+?<=?
 AND NOT EXISTS(SELECT 1 FROM automation_money WHERE state IN ('inflight','unknown','pending'))
	 AND (?<>'open' OR (SELECT COUNT(*) FROM automation_money WHERE action IN ('open','pro_open','pro5x_reserve_open') AND created_at>?)<?)
 AND EXISTS(SELECT 1 FROM automation_policy WHERE id=1 AND version=?)
 AND (?=0 OR NOT EXISTS(SELECT 1 FROM local_cdks WHERE card_id=? AND status IN ('reserved','review')))
	 AND (?<>'topup' OR (EXISTS(SELECT 1 FROM local_card_cycles WHERE card_id=?)
	 AND EXISTS(SELECT 1 FROM automation_card_lifecycle WHERE card_id=? AND retire_state='active')))
	 AND EXISTS(SELECT 1 FROM site_settings WHERE key='local_cdk_settings' AND value=?)`, id, action, cardID, amount, cost, before, scope, now, cost, p.MaxOperation, cost, now-86400, cost, p.DailyBudget, action, now-86400, p.DailyOpen, version, cardID, cardID, action, cardID, cardID, localRaw)
	if e != nil {
		autoAlert("money", 0, "无法预留资金预算，未进行操作。")
		return
	}
	n, _ = r.RowsAffected()
	if n != 1 {
		autoAlert("money", 0, "已达到自动资金预算、开卡数量限制或卡片被占用，本次未执行。")
		return
	}
	if action == "open" {
		if _, e = tx.Exec(`INSERT INTO automation_product_selections(operation_id,product_code,bin,selection_mode,selected_at)
		 VALUES(?,?,?,?,?)`, id, openProduct, openBIN, selectionMode, now); e != nil {
			autoAlert("money", 0, "无法记录自动开卡的卡头选择，未进行开卡。")
			return
		}
	}
	if e = tx.Commit(); e != nil {
		autoAlert("money", 0, "无法确认自动资金预算，未进行操作。")
		return
	}
	newCard := int64(0)
	if action == "topup" {
		e = cli.AutomationRecharge(ctx, cardID, amount, id)
	} else {
		newCard, e = cli.AutomationOpen(ctx, openProduct, p.First, p.Last, amount, id)
	}
	state := "pending"
	if e != nil {
		state = "unknown"
	}
	_, _ = db.DB.Exec("UPDATE automation_money SET state=?,result_card_id=? WHERE id=?", state, newCard, id)
	db.WriteAudit("automation", "card_"+action, fmt.Sprintf("operation=%s card=%d product=%s selection=%s reserved_minor=%d result=%s", id, cardID, openProduct, selectionMode, cost, state), "")
	if state == "unknown" {
		autoAlert("money", 0, "资金操作结果不明，自动资金操作与新充值已暂停。请到 Zovo 核对；本站不会自动重试或返还预算。")
	} else {
		// A provider-accepted request is normal progress, not an exception. Its
		// pending state remains visible in the operations table until the next
		// balance check, while Telegram stays quiet unless verification fails.
		autoResolve("money")
	}
}
func verifyMoneyOperations(inventory []cardplatform.CardChoice, p automationPolicy) {
	cfg := cardplatform.LoadConfig()
	verifyMoneyOperationsForScope(inventory, p, localHash(cfg.SiteBase+"|"+cfg.APIKey))
}
func verifyMoneyOperationsForScope(inventory []cardplatform.CardChoice, p automationPolicy, inventoryScope string) {
	currentConfig := cardplatform.LoadConfig()
	if localHash(currentConfig.SiteBase+"|"+currentConfig.APIKey) != inventoryScope {
		return
	}
	now := time.Now().Unix()
	rows, e := db.DB.Query("SELECT id,action,card_id,result_card_id,before_minor,amount_minor,created_at,scope,state FROM automation_money WHERE state IN ('pending','unknown') AND created_at<?", now-120)
	if e != nil {
		return
	}
	type pendingOp struct {
		id, action, scope, state         string
		card, result, before, amount, at int64
	}
	ops := []pendingOp{}
	for rows.Next() {
		var op pendingOp
		if rows.Scan(&op.id, &op.action, &op.card, &op.result, &op.before, &op.amount, &op.at, &op.scope, &op.state) == nil {
			ops = append(ops, op)
		}
	}
	rows.Close()
	scope := inventoryScope
	for _, op := range ops {
		if op.scope != scope {
			autoAlert("money", 0, "接入密钥已变更，旧资金操作必须使用原账号核对，未自动解除锁定。")
			continue
		}
		target := op.card
		if op.action == "open" || op.action == "pro5x_reserve_open" {
			target = op.result
		}
		matched := false
		// A provider-accepted pending request may be confirmed by its expected
		// post-recharge balance. An unknown request is stricter: a later unrelated
		// top-up could also raise the balance, so require an exact ledger entry.
		if op.state == "pending" {
			for _, card := range inventory {
				balance, ok := usdMinor(card.Balance)
				if card.ID == target && card.Status == "ACTIVE" && ok && balance >= op.before+op.amount {
					matched = true
					break
				}
			}
		}
		if !matched && (op.action == "topup" || op.action == "pro5x_reserve_topup") {
			// The card may be spent or archived before the next inventory scan. A
			// successful, exact recharge ledger entry is authoritative evidence
			// that the accepted request completed even if the current balance no
			// longer reaches the short-lived post-recharge high-water mark.
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			entries, _, ledgerErr := cardplatform.New(currentConfig).FinancePage(ctx, "recharge", op.card, 1)
			cancel()
			if ledgerErr == nil {
				maxDelay := int64((15 * time.Minute).Seconds())
				if op.state == "unknown" {
					// Some provider timeouts finish asynchronously many hours later.
					// Keep the recovery window bounded and require a unique exact
					// card/amount/status match so an unrelated manual top-up cannot
					// silently release the safety lock.
					maxDelay = int64((24 * time.Hour).Seconds())
				}
				matches := []cardplatform.FinanceEntry{}
				for _, entry := range entries {
					at, parseErr := time.Parse(time.RFC3339Nano, entry.At)
					status := strings.ToLower(strings.TrimSpace(entry.Status))
					if parseErr != nil || entry.ID <= 0 || entry.Amount == nil || *entry.Amount != op.amount ||
						(status != "success" && status != "succeeded" && status != "completed") ||
						at.Unix() < op.at-30 || at.Unix() > op.at+maxDelay {
						continue
					}
					matches = append(matches, entry)
				}
				if len(matches) == 1 {
					entry := matches[0]
					tx, txErr := db.DB.Begin()
					if txErr != nil {
						continue
					}
					if _, txErr = tx.Exec(`INSERT INTO automation_money_evidence(operation_id,scope,source,upstream_id,confirmed_at)
						 VALUES(?,?,?,?,?)`, op.id, op.scope, "card_recharge", entry.ID, now); txErr == nil {
						var result sql.Result
						result, txErr = tx.Exec("UPDATE automation_money SET state='balance_verified' WHERE id=? AND state IN ('pending','unknown')", op.id)
						if txErr == nil {
							var changed int64
							changed, txErr = result.RowsAffected()
							if changed != 1 {
								txErr = fmt.Errorf("money operation state changed")
							}
						}
					}
					if txErr == nil {
						txErr = tx.Commit()
					} else {
						_ = tx.Rollback()
					}
					if txErr == nil {
						matched = true
						db.WriteAudit("automation", "money_ledger_verified", fmt.Sprintf("operation=%s source=card_recharge upstream=%d", op.id, entry.ID), "")
					}
				}
			}
		}
		if !matched {
			if now-op.at > 1800 {
				autoAlert("money", 0, "资金请求超过 30 分钟仍未核查到预期余额，请在 Zovo 核对；不会自动重试。")
			}
			continue
		}
		var changed int64
		if op.action != "topup" || !moneyOperationHasEvidence(op.id) {
			var result sql.Result
			result, e = db.DB.Exec("UPDATE automation_money SET state='balance_verified' WHERE id=? AND state IN ('pending','unknown')", op.id)
			if e != nil {
				continue
			}
			changed, _ = result.RowsAffected()
		} else {
			changed = 1
		}
		if changed != 1 {
			continue
		}
		autoResolve("money")
		current, _, policyErr := readAutomationPolicy()
		if op.action == "open" && policyErr == nil && current.Enroll && !current.Paused {
			if e := enrollVerifiedCard(target); e != nil {
				autoAlert("enrollment", 0, "新卡余额已确认，但支付名单未更新；请检查名单或刷新后重试。")
			}
		}
	}
}

func moneyOperationHasEvidence(operationID string) bool {
	var found int
	return db.DB.QueryRow("SELECT 1 FROM automation_money_evidence WHERE operation_id=?", operationID).Scan(&found) == nil && found == 1
}

// reconcileCreatedCardEnrollment heals the administrator's persisted payment
// list after a browser draft, an older release, or a completed Pro 5X workflow
// leaves an eligible site-created card outside the static selection. The
// active-inventory and lifecycle checks keep prepared Pro 5X reserve cards,
// unfinished dedicated cards, archived cards, and exhausted cards isolated.
func reconcileCreatedCardEnrollment(inventory []cardplatform.CardChoice, p automationPolicy) {
	if !p.Enroll || p.Paused {
		return
	}
	active := map[int64]bool{}
	for _, card := range inventory {
		if card.ID > 0 && card.Status == "ACTIVE" {
			active[card.ID] = true
		}
	}
	rows, err := db.DB.Query(`SELECT card_id FROM (
		SELECT result_card_id AS card_id
		FROM automation_money
		WHERE action='open' AND state='balance_verified' AND result_card_id>0
		UNION
		SELECT p.card_id
		FROM pro_dedicated_orders p JOIN local_cdks c ON c.id=p.local_id
		WHERE p.card_id>0 AND p.state='completed'
		AND c.plan='pro_5x' AND c.status='consumed'
		UNION
		SELECT card_id
		FROM pro5x_card_policy
		WHERE completed_uses>=?
	) ORDER BY card_id`, pro5xUsesBeforePlusPool)
	if err != nil {
		autoAlert("enrollment", 0, "无法核对本站新开卡的支付名单，本轮未改动名单；稍后自动重试。")
		return
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			autoAlert("enrollment", 0, "无法读取本站新开卡的支付名单，本轮未改动名单；稍后自动重试。")
			return
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		autoAlert("enrollment", 0, "无法完整读取本站新开卡的支付名单，本轮未改动名单；稍后自动重试。")
		return
	}
	for _, id := range ids {
		if !active[id] {
			continue
		}
		capacity, capacityErr := localCardHasPaymentCapacity(id, 0)
		if capacityErr != nil {
			autoAlert("enrollment", 0, fmt.Sprintf("卡片 #%d 的使用周期无法核对，暂未加入支付名单；稍后自动重试。", id))
			return
		}
		if !capacity {
			continue
		}
		if err = enrollVerifiedCard(id); err != nil {
			autoAlert("enrollment", 0, fmt.Sprintf("卡片 #%d 已确认可用，但加入支付名单失败；稍后自动重试。", id))
			return
		}
	}
	autoResolve("enrollment")
}

// A full 20-card pool may release a slot held by a capped, idle card.
// Its usage and ledger history stay intact; no upstream card is closed or deleted.
func enrollVerifiedCard(target int64) error {
	raw, e := db.GetSetting("local_cdk_settings")
	if e != nil {
		return e
	}
	var settings localSettings
	if e = json.Unmarshal([]byte(raw), &settings); e != nil {
		return e
	}
	ids := localCardIDs(settings)
	for _, id := range ids {
		if id == target {
			return nil
		}
	}
	if len(ids) >= 20 {
		var exhausted map[int64]bool
		if settings.UseUpstreamCardLimit {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			exhausted, e = upstreamExhaustedCards(ctx, cardplatform.NewFromSettings())
			if e != nil {
				return e
			}
		}
		for index, id := range ids {
			capacity, err := localCardHasPaymentCapacity(id, localPaymentLimit(settings))
			if err != nil {
				return err
			}
			var busy int
			if err = db.DB.QueryRow("SELECT COUNT(*) FROM local_cdks WHERE card_id=? AND status IN ('reserved','review')", id).Scan(&busy); err != nil {
				return err
			}
			if (!capacity || exhausted[id]) && busy == 0 {
				ids = append(ids[:index:index], ids[index+1:]...)
				break
			}
		}
	}
	if len(ids) >= 20 {
		return fmt.Errorf("card pool full")
	}
	settings.CardIDs = append(ids, target)
	settings.CardID = 0
	next, _ := json.Marshal(settings)
	result, e := db.DB.Exec("UPDATE site_settings SET value=?,updated_at=CURRENT_TIMESTAMP WHERE key='local_cdk_settings' AND value=?", string(next), raw)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return fmt.Errorf("settings changed")
	}
	db.WriteAudit("automation", "enroll_opened_card", strconv.FormatInt(target, 10), "")
	autoResolve("enrollment")
	return nil
}
