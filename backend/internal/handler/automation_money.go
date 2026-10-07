package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
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

// Keep at least two healthy working cards. When the available healthy set is
// at or below two, replenish until three are available. Pending/unknown
// funding, an opened-but-not-enrolled card, the rolling budget and the daily
// opening limit continue to prevent repeated openings.
const automationReadyCardFloor = 2

// Keep a small funded working set. Other selected cards remain available for
// rotation, but are only topped up when the working set drops to two or fewer.
const automationFundedCardTarget = 3

const (
	cardRenewalReminderWindow = 72 * time.Hour
	cardPostChargeTopupDelay  = 24 * time.Hour
	// Upstream cancellation is eventually consistent. Do not surface a card
	// alert while a fresh pending/processing result is still within this grace
	// period; the order-level poll will normally settle it first.
	cardRenewalPendingGrace = 10 * time.Minute
)

type cardRenewalState struct {
	nextDue       int64
	lastActivated int64
}

// A card is protected until its latest consumed order explicitly confirms
// that renewal is cancelled (or that no renewal was requested).
func cardRenewalProtected(cardID int64) (bool, string) {
	var renewal string
	err := db.DB.QueryRow(`SELECT w.renewal
		FROM automation_watch w JOIN local_cdks c ON c.id=w.local_id
		WHERE c.card_id=? AND c.status='consumed'
		ORDER BY w.checked_at DESC,w.local_id DESC LIMIT 1`, cardID).Scan(&renewal)
	if err == sql.ErrNoRows {
		return false, ""
	}
	if err != nil {
		return true, "查询失败"
	}
	switch strings.ToLower(strings.TrimSpace(renewal)) {
	case "success", "not_requested":
		return false, renewal
	case "":
		return true, "未确认"
	default:
		return true, renewal
	}
}

func loadCardRenewalStates(inventory []cardplatform.CardChoice, now int64) map[int64]cardRenewalState {
	states := make(map[int64]cardRenewalState, len(inventory))
	for _, card := range inventory {
		var state cardRenewalState
		_ = db.DB.QueryRow(`SELECT COALESCE(MIN(CASE WHEN subscription_expires_at>? THEN subscription_expires_at ELSE 0 END),0),
			COALESCE(MAX(activated_at),0) FROM local_cdks
			WHERE card_id=? AND status='consumed' AND subscription_expires_at>?`, now, card.ID, now).
			Scan(&state.nextDue, &state.lastActivated)
		states[card.ID] = state
	}
	return states
}

// Cards close to a known subscription renewal remain usable, but funding is
// held for an explicit owner decision. Missing cards do not produce reminders.
func maintainCardRenewalAlerts(now int64) {
	scopeCfg := cardplatform.LoadConfig()
	scope := localHash(scopeCfg.SiteBase + "|" + scopeCfg.APIKey)
	rows, err := db.DB.Query(`SELECT c.card_id,MIN(c.subscription_expires_at),COUNT(*),
		COALESCE(GROUP_CONCAT(NULLIF(c.email,''),', '),'')
		FROM local_cdks c
		JOIN automation_inventory_snapshot s ON s.scope=? AND s.card_id=c.card_id AND s.present=1
		JOIN automation_card_lifecycle l ON l.card_id=c.card_id AND l.retire_state='active'
		WHERE c.status='consumed' AND c.card_id>0 AND c.subscription_expires_at>? AND c.subscription_expires_at<=?
		GROUP BY c.card_id`, scope, now, now+int64(cardRenewalReminderWindow/time.Second))
	if err != nil {
		return
	}
	active := map[string]bool{}
	scannedCards, protectedCards := int64(0), int64(0)
	for rows.Next() {
		var cardID, due, count int64
		var emails string
		if rows.Scan(&cardID, &due, &count, &emails) != nil {
			continue
		}
		key := fmt.Sprintf("card-renewal:%d", cardID)
		active[key] = true
		autoAlert(key, 0, fmt.Sprintf("支付卡 #%d 有 %d 个客户将在约 %s 内到期（%s）；卡片仍保持可支付，请确认是否补款并继续使用。", cardID, count, time.Until(time.Unix(due, 0)).Round(time.Hour), emails))
	}
	rows.Close()
	// Surface cards whose latest renewal result is not explicitly cancelled.
	// Such cards are protected from temporary 5X and ordinary health-card
	// funding until the upstream status is confirmed.
	rows, err = db.DB.Query(`SELECT s.card_id FROM automation_inventory_snapshot s
		JOIN automation_card_lifecycle l ON l.card_id=s.card_id AND l.retire_state='active'
		WHERE s.scope=? AND s.present=1`, scope)
	if err == nil {
		for rows.Next() {
			var cardID int64
			if rows.Scan(&cardID) != nil {
				continue
			}
			scannedCards++
			protected, status := cardRenewalProtected(cardID)
			if !protected {
				continue
			}
			if renewalStatusAwaitingUpstream(status) {
				var checkedAt int64
				if err := db.DB.QueryRow(`SELECT COALESCE(MAX(w.checked_at),0)
					FROM automation_watch w JOIN local_cdks c ON c.id=w.local_id
					WHERE c.card_id=? AND c.status='consumed'`, cardID).Scan(&checkedAt); err == nil &&
					checkedAt > 0 && now-checkedAt < int64(cardRenewalPendingGrace/time.Second) {
					continue
				}
			}
			protectedCards++
			key := fmt.Sprintf("card-renewal-state:%d", cardID)
			active[key] = true
			autoAlert(key, 0, fmt.Sprintf("支付卡 #%d 的自动续费状态为“%s”，已保护；不会用于临时 5X 或普通补款。", cardID, status))
		}
		rows.Close()
	}
	_, _ = db.DB.Exec(`CREATE TABLE IF NOT EXISTS operations_renewal_scan(
		id INTEGER PRIMARY KEY CHECK(id=1),checked_at INTEGER NOT NULL DEFAULT 0,
		scanned INTEGER NOT NULL DEFAULT 0,protected INTEGER NOT NULL DEFAULT 0)`)
	_, _ = db.DB.Exec(`INSERT INTO operations_renewal_scan(id,checked_at,scanned,protected)
		VALUES(1,?,?,?) ON CONFLICT(id) DO UPDATE SET checked_at=excluded.checked_at,
		scanned=excluded.scanned,protected=excluded.protected`, now, scannedCards, protectedCards)
	rows, err = db.DB.Query("SELECT alert_key FROM automation_alerts WHERE alert_key LIKE 'card-renewal:%' AND resolved=0")
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if rows.Scan(&key) == nil && !active[key] {
			autoResolve(key)
		}
	}
	rows.Close()
	rows, err = db.DB.Query("SELECT alert_key FROM automation_alerts WHERE alert_key LIKE 'card-renewal-state:%' AND resolved=0")
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if rows.Scan(&key) == nil && !active[key] {
			autoResolve(key)
		}
	}
}

func shouldOpenAutomationCard(ready int) bool {
	return ready <= automationReadyCardFloor
}

// hasPendingPlusDemand prevents proactive card creation. A Plus card is only
// opened when there is an actual reserved/review order that may need another
// healthy card; dedicated 5X orders use their own reserve path.
func hasPendingPlusDemand() bool {
	var count int
	err := db.DB.QueryRow(`SELECT COUNT(*) FROM local_cdks
		WHERE status IN ('reserved','review')
		  AND plan NOT IN ('pro_5x','pro_5x_cl')`).Scan(&count)
	return err == nil && count > 0
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

// persistAutomationInventory records only a fully completed inventory scan.
// It never deletes historical card references: cards absent from the latest
// Zovo response are marked missing so an old funding record cannot be mistaken
// for a currently usable card.
func persistAutomationInventory(inventory []cardplatform.CardChoice, scope string, now int64) error {
	if strings.TrimSpace(scope) == "" || now <= 0 {
		return fmt.Errorf("invalid inventory snapshot")
	}
	tx, err := db.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE automation_inventory_snapshot SET present=0,provider_status='DELETED',synced_at=? WHERE scope=?", now, scope); err != nil {
		return err
	}
	// Ensure cards referenced only by historical money operations also receive
	// an explicit missing state after this authoritative full scan.
	if _, err = tx.Exec(`INSERT OR IGNORE INTO automation_inventory_snapshot
		(scope,card_id,present,synced_at,last_seen_at)
		SELECT scope,CASE WHEN action IN ('open','pro5x_reserve_open') AND result_card_id>0 THEN result_card_id ELSE card_id END,
		0,?,0 FROM automation_money
		WHERE scope=? AND CASE WHEN action IN ('open','pro5x_reserve_open') AND result_card_id>0 THEN result_card_id ELSE card_id END>0`, now, scope); err != nil {
		return err
	}
	for _, card := range inventory {
		if card.ID <= 0 {
			return fmt.Errorf("invalid card in inventory snapshot")
		}
		if _, err = tx.Exec(`INSERT INTO automation_inventory_snapshot
			(scope,card_id,present,last4,product_code,provider_status,synced_at,last_seen_at)
			VALUES(?,?,1,?,?,?,?,?)
			ON CONFLICT(scope,card_id) DO UPDATE SET
			present=1,last4=excluded.last4,product_code=excluded.product_code,
			provider_status=excluded.provider_status,synced_at=excluded.synced_at,
			last_seen_at=excluded.last_seen_at`, scope, card.ID, card.Last4, card.Product, card.Status, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// stopFundingForMissingCards closes only pending/unknown recharge operations
// whose card disappeared from a complete provider inventory scan.  A deleted
// card cannot receive another recharge, so leaving the operation pending would
// keep producing misleading funding alerts and would block future work on a
// card that no longer exists.  The terminal state is deliberately distinct
// from balance_verified/no_charge_verified: it records that the card vanished
// without claiming that an already-submitted provider charge was refunded.
func stopFundingForMissingCards(scope string, now int64) {
	if strings.TrimSpace(scope) == "" || now <= 0 {
		return
	}
	// Clear card-scoped alerts even when the original operation was already
	// archived by an older release. The full inventory scan is the evidence that
	// this card is no longer a funding target.
	missing, err := db.DB.Query(`SELECT card_id FROM automation_inventory_snapshot
		WHERE scope=? AND present=0 AND card_id>0`, scope)
	if err == nil {
		missingIDs := []int64{}
		for missing.Next() {
			var cardID int64
			if missing.Scan(&cardID) == nil {
				missingIDs = append(missingIDs, cardID)
			}
		}
		missing.Close()
		for _, cardID := range missingIDs {
			autoResolve(fmt.Sprintf("money-card:%d", cardID))
			autoResolve(fmt.Sprintf("topup:%d", cardID))
			autoResolve(fmt.Sprintf("card-renewal:%d", cardID))
			autoResolve(fmt.Sprintf("card-renewal-state:%d", cardID))
			// A provider-deleted card is no longer an eligible local lifecycle
			// target. Close only this card's lifecycle row; do not alter global
			// automation switches or other cards.
			_, _ = db.DB.Exec(`UPDATE automation_card_lifecycle
				SET phase='closed',retire_state='closed',retire_reason='provider_deleted',
				updated_at=?,closed_at=CASE WHEN closed_at=0 THEN ? ELSE closed_at END
				WHERE card_id=?`, now, now, cardID)
		}
	}
	rows, err := db.DB.Query(`SELECT m.id,
		CASE WHEN m.action='pro5x_reserve_topup' AND m.result_card_id>0 THEN m.result_card_id ELSE m.card_id END,
		m.action
		FROM automation_money m
		LEFT JOIN automation_inventory_snapshot s
			ON s.scope=m.scope AND s.card_id=CASE WHEN m.action='pro5x_reserve_topup' AND m.result_card_id>0 THEN m.result_card_id ELSE m.card_id END
			AND s.present=1
		WHERE m.scope=? AND m.state IN ('inflight','pending','unknown')
			AND m.action IN ('topup','pro5x_reserve_topup') AND s.card_id IS NULL`, scope)
	if err != nil {
		return
	}
	type missingFunding struct {
		id, action string
		cardID     int64
	}
	operations := []missingFunding{}
	for rows.Next() {
		var id string
		var cardID int64
		var action string
		if rows.Scan(&id, &cardID, &action) != nil {
			continue
		}
		operations = append(operations, missingFunding{id: id, action: action, cardID: cardID})
	}
	rows.Close()
	for _, op := range operations {
		result, updateErr := db.DB.Exec(`UPDATE automation_money
			SET state='card_deleted_no_refill'
			WHERE id=? AND scope=? AND state IN ('inflight','pending','unknown')`, op.id, scope)
		if updateErr != nil {
			continue
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			continue
		}
		key := fmt.Sprintf("money-card:%d", op.cardID)
		autoResolve(key)
		// Older releases used a topup:<card> key for pricing/amount warnings;
		// clear it as well so a card that is gone does not leave a stale alert.
		autoResolve(fmt.Sprintf("topup:%d", op.cardID))
		db.WriteAudit("automation", "funding_skipped_deleted_card", fmt.Sprintf("operation=%s card=%d action=%s state=card_deleted_no_refill at=%d", op.id, op.cardID, op.action, now), "")
	}
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
	if e == nil {
		if snapshotErr := persistAutomationInventory(inventory, scope, now); snapshotErr != nil {
			e = &inventoryScanError{"本地卡片同步快照写入失败"}
		}
	}
	recordInventoryScan(e)
	if e != nil {
		return
	}
	// The inventory is now authoritative. Do this before normal reconciliation
	// so a removed card cannot emit the old 30-minute funding warning or be
	// considered for another recharge in the same cycle.
	stopFundingForMissingCards(scope, now)
	renewalStates := loadCardRenewalStates(inventory, now)
	verifyMoneyOperationsForScope(inventory, p, scope)
	if !dedicatedMoneyBlocked() {
		// Do not clear the shared money alert merely because this inventory read
		// succeeded. The same key also carries pricing, budget and unresolved
		// funding warnings; those must remain visible until their own evidence
		// resolves them. Inventory-specific legacy warnings are migrated by
		// recordInventoryScan above.
		resolveLegacyMoneyAlert(legacyInventoryAlert)
	}
	if uncertain > 0 && dedicatedMoneyBlocked() {
		var remaining int
		_ = db.DB.QueryRow("SELECT COUNT(*) FROM automation_money WHERE state IN ('inflight','unknown') AND action IN ('open','pro_open')").Scan(&remaining)
		if remaining > 0 {
			autoAlert("money", 0, "存在未确认的开卡资金操作，已暂停新的开卡；其他卡片的补款会按卡片范围继续核对。请先在 Zovo 核对，本站不会自动重试。")
		}
	}
	reconcilePro5xReserve(inventory)
	reconcileCreatedCardEnrollment(inventory, p)
	// A pending or uncertain provider-accepted money operation is reconciled in
	// this cycle. It is intentionally not a global stop: only the card (or the
	// same opening operation) remains blocked by the reservation predicates.
	// Unresolved opening operations only block another opening. Funding a
	// different healthy card continues; the reservation SQL below retains the
	// opening-specific guard and the card-specific guard for top-ups.
	var products []cardplatform.AutomationProduct
	productErr := retryAutomationRead(ctx, func(readCtx context.Context) error {
		var err error
		products, err = cli.AutomationProducts(readCtx)
		return err
	})
	if productErr != nil {
		// Product fees are a read-only prerequisite. A transient upstream
		// timeout must not poison the shared money alert forever, but we must
		// still fail closed for this cycle and never submit a funding action.
		autoAlert("money_products", 0, "Zovo 产品费率"+automationReadReason(productErr)+"，本轮未进行资金操作；查询恢复后自动解除。")
		return
	}
	autoResolve("money_products")
	resolveLegacyMoneyAlert("无法核查产品费率，未进行资金操作。")
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
	// Keep the single shared Pro 5X reserve funded for both Philippines and
	// Chile orders. A claimed reserve is replenished instead of opening another
	// card for the next order.
	if maintainPro5xReserve(ctx, cli, inventory, p, version, scope, productMap, now) {
		return
	}
	reconcileRetiredCards(inventory, now)
	if p.Retire && processQueuedCardRetirement(ctx, cli, inventory, scope, version, now) {
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
	if cfg.MinCardBalanceMinor <= 0 || (p.Open && p.InitAmount < cfg.MinCardBalanceMinor) {
		autoAlert("money", 0, "支付卡余额门槛无效或开卡金额低于该门槛，未执行资金操作。")
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
	// 5X cards remain dedicated and are always excluded from the ordinary Plus
	// funding pool. The reserve card is also excluded while it is claimed.
	rows, selectErr := db.DB.Query(`SELECT p.card_id
		FROM pro_dedicated_orders p JOIN local_cdks c ON c.id=p.local_id
		WHERE p.card_id>0 AND p.state='completed' AND c.plan IN ('pro_5x','pro_5x_cl') AND c.status='consumed'`)
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
	rows, selectErr = db.DB.Query("SELECT card_id FROM pro5x_card_policy")
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
		// The payment-card balance requirement is the dynamic funding goal. The
		// automation target setting is retained only for database compatibility;
		// it must not turn the USD 30 card ceiling into a fill-to target. Recharge
		// only the shortfall needed for Plus, rounded up to the provider's USD 10
		// recharge minimum, and never cross the saved card ceiling.
		fundingFloor := cfg.MinCardBalanceMinor
		if p.Threshold > fundingFloor {
			fundingFloor = p.Threshold
		}
		// Do not pre-fund every low-balance card. Once two ordinary cards are
		// ready, leave the rest low and replenish them only when a working slot
		// becomes necessary.
		if len(ready) < automationFundedCardTarget {
			// Health-first order: cards without a scheduled renewal, then cards
			// whose next renewal is farthest away.
			sort.SliceStable(inventory, func(i, j int) bool {
				left, right := renewalStates[inventory[i].ID], renewalStates[inventory[j].ID]
				if left.nextDue == 0 {
					return right.nextDue != 0
				}
				if right.nextDue == 0 {
					return false
				}
				return left.nextDue > right.nextDue
			})
			for _, card := range inventory {
				if protected, _ := cardRenewalProtected(card.ID); protected {
					continue
				}
				state := renewalStates[card.ID]
				if state.lastActivated > 0 && now-state.lastActivated < int64(cardPostChargeTopupDelay/time.Second) {
					continue
				}
				if state.nextDue > now && state.nextDue-now <= int64(cardRenewalReminderWindow/time.Second) {
					continue
				}
			var reserveID int64
			var reserveState string
			if err := db.DB.QueryRow("SELECT card_id,state FROM pro5x_card_reserve WHERE id=1").Scan(&reserveID, &reserveState); err == nil && reserveID == card.ID && reserveState != "empty" {
				continue
			}
			var dedicated int
			if err := db.DB.QueryRow(`SELECT COUNT(*)
				FROM pro_dedicated_orders p JOIN local_cdks c ON c.id=p.local_id
				WHERE p.card_id=? AND NOT (p.state='completed' AND c.plan IN ('pro_5x','pro_5x_cl') AND c.status='consumed')`, card.ID).Scan(&dedicated); err != nil {
				return
			}
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
			if !ok || balance >= fundingFloor {
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
			amount = fundingFloor - balance
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
	}
	if action == "" && p.Open && !dedicatedMoneyBlocked() && hasPendingPlusDemand() && shouldOpenAutomationCard(len(ready)) {
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
			// The upstream product order is authoritative: it reflects the
			// card-head priority configured in Zovo. Local success-rate ranking
			// remains available for reporting, but must not override that choice.
			product, selectionMode, e = selectUpstreamAutomationProduct(products, p.InitAmount)
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
		AND NOT EXISTS(SELECT 1 FROM automation_money WHERE state IN ('inflight','unknown','pending') AND action IN ('open','pro_open'))
		AND (?<>'open' OR (SELECT COUNT(*) FROM automation_money WHERE action IN ('open','pro_open','pro5x_reserve_open') AND created_at>?)<?)
		AND (?<>'topup' OR NOT EXISTS(SELECT 1 FROM automation_money WHERE state IN ('inflight','unknown','pending') AND (card_id=? OR result_card_id=?)))
		AND EXISTS(SELECT 1 FROM automation_policy WHERE id=1 AND version=?)
 AND (?=0 OR NOT EXISTS(SELECT 1 FROM local_cdks WHERE card_id=? AND status IN ('reserved','review')))
	 AND (?<>'topup' OR (EXISTS(SELECT 1 FROM local_card_cycles WHERE card_id=?)
	 AND EXISTS(SELECT 1 FROM automation_card_lifecycle WHERE card_id=? AND retire_state='active')))
		AND EXISTS(SELECT 1 FROM site_settings WHERE key='local_cdk_settings' AND value=?)`, id, action, cardID, amount, cost, before, scope, now, cost, p.MaxOperation, cost, now-86400, cost, p.DailyBudget, action, now-86400, p.DailyOpen, action, cardID, cardID, version, cardID, cardID, action, cardID, cardID, localRaw)
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
		if action == "open" || action == "pro_open" {
			autoAlert("money", 0, "开卡资金操作结果不明，新的开卡会暂停；其他卡片补款不受影响。请到 Zovo 核对，本站不会自动重试。")
		} else {
			autoAlert(fmt.Sprintf("money-card:%d", cardID), 0, fmt.Sprintf("卡片 #%d 补款结果不明，仅锁定该卡；其他卡片和新专卡订单不受影响。请到 Zovo 核对，本站不会自动重试。", cardID))
		}
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
	// A legacy scope mismatch is intentionally kept as a safety hold, but it is
	// not a notification-worthy event on every reconciliation cycle.  Older
	// releases used the generic "money" alert key, so clear only that exact
	// legacy message; unrelated money alerts remain untouched.
	_, _ = db.DB.Exec("UPDATE automation_alerts SET resolved=1 WHERE alert_key='money' AND message LIKE '接入密钥已变更%'")
	for _, op := range ops {
		if op.scope != scope {
			// Keep the operation locked and auditable, but do not repeatedly notify
			// the administrator.  The mismatch is not evidence of a human key
			// change; it only means this old operation belongs to another config
			// scope and must be checked with its original account.
			continue
		}
		target := op.card
		if op.action == "open" || op.action == "pro5x_reserve_open" {
			target = op.result
		}
		matched := false
		ledgerComplete := false
		exactLedgerEntry := false
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
				// Card recharge history is returned as one complete list (bounded and
				// validated by the client), so an old operation can also be proven to
				// have produced no ledger entry at all.
				ledgerComplete = true
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
						at.Unix() < op.at-30 || at.Unix() > op.at+maxDelay {
						continue
					}
					exactLedgerEntry = true
					if status != "success" && status != "succeeded" && status != "completed" {
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
		if !matched && op.state == "unknown" && op.action == "topup" && now-op.at >= int64((24*time.Hour)/time.Second) && ledgerComplete && !exactLedgerEntry {
			// After a full day, two independent provider facts are sufficient to
			// prove that a timed-out top-up did not charge: the complete card ledger
			// has no operation-window entry and the live card balance never rose
			// above its pre-request value. Record that negative evidence before
			// releasing the safety lock. Ambiguous ledger rows remain locked.
			var liveBalance int64
			balanceConfirmed := false
			for _, card := range inventory {
				balance, ok := usdMinor(card.Balance)
				if card.ID == op.card && card.Status == "ACTIVE" && ok && balance <= op.before {
					liveBalance = balance
					balanceConfirmed = true
					break
				}
			}
			if balanceConfirmed {
				tx, txErr := db.DB.Begin()
				if txErr == nil {
					_, txErr = tx.Exec(`INSERT INTO automation_money_nocharge_evidence
						(operation_id,scope,card_id,balance_minor,checked_at,reason)
						VALUES(?,?,?,?,?,?)`, op.id, op.scope, op.card, liveBalance, now, "complete recharge ledger has no matching entry after 24h; live balance did not increase")
					if txErr == nil {
						var result sql.Result
						result, txErr = tx.Exec("UPDATE automation_money SET state='no_charge_verified' WHERE id=? AND state='unknown'", op.id)
						if txErr == nil {
							changed, rowsErr := result.RowsAffected()
							if rowsErr != nil || changed != 1 {
								txErr = fmt.Errorf("money operation state changed")
							}
						}
					}
					if txErr == nil {
						txErr = tx.Commit()
					} else {
						_ = tx.Rollback()
					}
				}
				if txErr == nil {
					db.WriteAudit("automation", "money_nocharge_verified", fmt.Sprintf("operation=%s card=%d balance=%d", op.id, op.card, liveBalance), "")
					var remaining int
					_ = db.DB.QueryRow("SELECT COUNT(*) FROM automation_money WHERE state IN ('inflight','pending','unknown')").Scan(&remaining)
					if remaining == 0 {
						autoResolve("money")
					}
					continue
				}
			}
		}
		if !matched {
			if now-op.at > 1800 {
				present := false
				for _, card := range inventory {
					if card.ID == target {
						present = true
						break
					}
				}
				if !present {
					key := "money"
					if op.action == "topup" || op.action == "pro5x_reserve_topup" {
						key = fmt.Sprintf("money-card:%d", target)
					}
					autoAlert(key, 0, fmt.Sprintf("资金请求超过 30 分钟仍未找到成功流水；Zovo 当前卡片列表已不含卡片 ID #%d。该编号是历史内部 ID，不是卡号尾号；请在充值记录或资金流水核对，不会自动重试。", target))
				} else {
					key := "money"
					if op.action == "topup" || op.action == "pro5x_reserve_topup" {
						key = fmt.Sprintf("money-card:%d", target)
					}
					autoAlert(key, 0, "资金请求超过 30 分钟仍未核查到预期余额，请在 Zovo 充值记录或资金流水核对；不会自动重试。")
				}
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
	autoResolve("money-scope-mismatch")
}

func moneyOperationHasEvidence(operationID string) bool {
	var found int
	return db.DB.QueryRow("SELECT 1 FROM automation_money_evidence WHERE operation_id=?", operationID).Scan(&found) == nil && found == 1
}

// reconcileCreatedCardEnrollment heals the administrator's persisted payment
// list after a browser draft, an older release, or a completed Pro 5X workflow
// leaves an eligible site-created card outside the static selection. The
// active-inventory and lifecycle checks keep the currently prepared shared
// reserve, unfinished dedicated cards, archived cards, and exhausted cards isolated.
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
		AND c.plan IN ('pro_5x','pro_5x_cl') AND c.status='consumed'
		UNION
		SELECT card_id
		FROM pro5x_card_policy
	) ORDER BY card_id`)
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
