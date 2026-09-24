package handler

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

const pro5xReserveProduct = "P5378OX"

type pro5xReserve struct {
	CardID    int64
	MoneyID   string
	State     string
	CreatedAt int64
	UpdatedAt int64
}

func loadPro5xReserve() (pro5xReserve, error) {
	var r pro5xReserve
	err := db.DB.QueryRow(`SELECT card_id,money_id,state,created_at,updated_at
		FROM pro5x_card_reserve WHERE id=1`).Scan(&r.CardID, &r.MoneyID, &r.State, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

func findInventoryCard(inventory []cardplatform.CardChoice, cardID int64) (int64, bool) {
	for _, card := range inventory {
		balance, ok := usdMinor(card.Balance)
		if card.ID == cardID && card.Status == "ACTIVE" && ok {
			return balance, true
		}
	}
	return 0, false
}

func reconcilePro5xReserve(inventory []cardplatform.CardChoice) {
	r, err := loadPro5xReserve()
	if err != nil {
		autoAlert("pro5x_reserve", 0, "无法读取 Pro 5X 专卡状态，已停止自动准备专卡。")
		return
	}
	if r.State == "empty" {
		autoResolve("pro5x_reserve")
		return
	}
	if r.State == "review" {
		autoAlert("pro5x_reserve", 0, "Pro 5X 专卡准备结果需要核对；系统不会重复开卡。")
		return
	}
	if r.State == "funding" {
		var moneyState string
		if err = db.DB.QueryRow("SELECT state FROM automation_money WHERE id=?", r.MoneyID).Scan(&moneyState); err != nil {
			autoAlert("pro5x_reserve", 0, "Pro 5X 专卡资金记录缺失，已停止自动准备专卡。")
			return
		}
		if moneyState == "unknown" {
			_, _ = db.DB.Exec("UPDATE pro5x_card_reserve SET state='review',updated_at=? WHERE id=1 AND state='funding'", time.Now().Unix())
			autoAlert("pro5x_reserve", 0, "Pro 5X 专卡开卡或充值结果不明，请核对上游；系统不会重复开卡。")
			return
		}
		if moneyState != "balance_verified" {
			return
		}
		balance, ok := findInventoryCard(inventory, r.CardID)
		if ok && balance >= pro5xInitialMinor {
			tx, updateErr := db.DB.Begin()
			if updateErr != nil {
				return
			}
			defer tx.Rollback()
			result, updateErr := tx.Exec(`UPDATE pro5x_card_reserve SET state='ready',updated_at=?
				WHERE id=1 AND state='funding' AND card_id=? AND money_id=?`, time.Now().Unix(), r.CardID, r.MoneyID)
			changed := int64(0)
			if updateErr == nil {
				changed, _ = result.RowsAffected()
			}
			if updateErr == nil && changed == 1 {
				// Preparing the replacement reserve consumes this funding cycle.
				// Wake the next 30-second tick so the released Pro 5X card can
				// immediately receive its ordinary Plus balance maintenance.
				_, updateErr = tx.Exec("UPDATE automation_runtime SET money_after=0 WHERE id=1")
			}
			if updateErr == nil && changed == 1 && tx.Commit() == nil {
				autoResolve("pro5x_reserve")
			}
		}
		return
	}
	if r.State == "ready" {
		balance, ok := findInventoryCard(inventory, r.CardID)
		if !ok || balance < pro5xInitialMinor {
			_, _ = db.DB.Exec(`UPDATE pro5x_card_reserve SET state='review',updated_at=?
				WHERE id=1 AND state='ready' AND card_id=?`, time.Now().Unix(), r.CardID)
			autoAlert("pro5x_reserve", 0, fmt.Sprintf("Pro 5X 预备专卡 #%d 当前余额不足 $100 或状态异常，请核对后再启用。", r.CardID))
			return
		}
		autoResolve("pro5x_reserve")
	}
}

// maintainPro5xReserve keeps one verified USD 100 card available. It returns
// true when it initiated an upstream opening, so the caller performs no other
// money operation in the same automation cycle.
func maintainPro5xReserve(ctx context.Context, cli *cardplatform.Client, inventory []cardplatform.CardChoice, p automationPolicy, policyVersion int64, scope string, productMap map[string]cardplatform.AutomationProduct, now int64) bool {
	settings := settingsForLocalPlan(readLocalSettings(), "pro_5x")
	if !settings.Enabled || !settings.ProDedicatedEnabled || !p.Sync || p.Paused || !p.Open || p.First == "" || p.Last == "" || p.DailyBudget <= 0 || p.DailyOpen <= 0 {
		return false
	}
	reserve, err := loadPro5xReserve()
	if err != nil || reserve.State != "empty" {
		return false
	}
	// A newly governed 5X card stays dedicated for three successful 5X
	// upgrades. Between uses, refill the same card to $100 instead of opening a
	// replacement or releasing it into the Plus pool.
	rows, err := db.DB.Query(`SELECT card_id FROM pro5x_card_policy
		WHERE completed_uses>0 AND completed_uses<? ORDER BY updated_at DESC,card_id`, pro5xUsesBeforePlusPool)
	if err != nil {
		return false
	}
	reuseID := int64(0)
	for rows.Next() {
		var id int64
		if rows.Scan(&id) != nil {
			continue
		}
		for _, card := range inventory {
			if card.ID == id && card.Status == "ACTIVE" {
				reuseID = id
				break
			}
		}
		if reuseID > 0 {
			break
		}
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return false
	}
	if err = rows.Close(); err != nil {
		return false
	}
	if reuseID > 0 {
		var card cardplatform.CardChoice
		for _, candidate := range inventory {
			if candidate.ID == reuseID {
				card = candidate
				break
			}
		}
		balance, balanceOK := usdMinor(card.Balance)
		if !balanceOK || balance > pro5xInitialMinor {
			autoAlert("pro5x_reserve", 0, fmt.Sprintf("Pro 5X 三次专用卡 #%d 的余额无法核对，未开新卡也未补款。", reuseID))
			return true
		}
		if balance == pro5xInitialMinor {
			result, updateErr := db.DB.Exec(`UPDATE pro5x_card_reserve
				SET card_id=?,money_id='',state='ready',created_at=?,updated_at=?
				WHERE id=1 AND state='empty'`, reuseID, now, now)
			if changed, _ := result.RowsAffected(); updateErr == nil && changed == 1 {
				autoResolve("pro5x_reserve")
				return true
			}
			return false
		}
		amount := pro5xInitialMinor - balance
		product, ok := productMap[card.Product]
		cost, costOK := productCost(product, amount, false)
		if !ok || !costOK || product.RechargeFee == nil || *product.RechargeFee != 0 {
			autoAlert("pro5x_reserve", 0, fmt.Sprintf("Pro 5X 三次专用卡 #%d 的补款费率或金额限制不符合规则，未补款。", reuseID))
			return true
		}
		spendable, spendErr := cli.AutomationSpendable(ctx)
		wallet, walletOK := usdMinor(spendable)
		if spendErr != nil || !walletOK || wallet-cost < p.WalletFloor {
			autoAlert("pro5x_reserve", 0, fmt.Sprintf("平台可消费余额不足以补充 Pro 5X 三次专用卡 #%d，未补款。", reuseID))
			return true
		}
		operationID, randomErr := localRandom()
		if randomErr != nil {
			return false
		}
		operationID = "pro5x-reserve-topup-" + operationID
		dailyBudget := p.DailyBudget
		if dailyBudget > proDailyMaximum {
			dailyBudget = proDailyMaximum
		}
		tx, txErr := db.DB.Begin()
		if txErr != nil {
			return false
		}
		defer tx.Rollback()
		result, txErr := tx.Exec(`INSERT INTO automation_money
			(id,action,card_id,amount_minor,reserved_minor,before_minor,scope,state,created_at)
			SELECT ?,'pro5x_reserve_topup',?,?,?,?,?,'inflight',?
			WHERE COALESCE((SELECT SUM(reserved_minor) FROM automation_money WHERE created_at>?),0)+?<=?
			AND NOT EXISTS(SELECT 1 FROM automation_money WHERE state IN ('inflight','unknown','pending'))
			AND EXISTS(SELECT 1 FROM automation_policy WHERE id=1 AND version=?)
			AND EXISTS(SELECT 1 FROM pro5x_card_reserve WHERE id=1 AND state='empty')`,
			operationID, reuseID, amount, cost, balance, scope, now, now-86400, cost, dailyBudget, policyVersion)
		if txErr != nil {
			return false
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			autoAlert("pro5x_reserve", 0, "Pro 5X 三次专用卡受每日预算或待核对资金操作限制，本轮未补款。")
			return true
		}
		result, txErr = tx.Exec(`UPDATE pro5x_card_reserve
			SET card_id=?,money_id=?,state='funding',created_at=?,updated_at=?
			WHERE id=1 AND state='empty'`, reuseID, operationID, now, now)
		if txErr != nil {
			return false
		}
		changed, _ = result.RowsAffected()
		if changed != 1 || tx.Commit() != nil {
			return false
		}
		rechargeErr := cli.AutomationRecharge(ctx, reuseID, amount, operationID)
		state := "pending"
		if rechargeErr != nil {
			state = "unknown"
		}
		_, _ = db.DB.Exec("UPDATE automation_money SET state=? WHERE id=?", state, operationID)
		if state == "unknown" {
			_, _ = db.DB.Exec("UPDATE pro5x_card_reserve SET state='review',updated_at=? WHERE id=1 AND money_id=?", time.Now().Unix(), operationID)
			autoAlert("pro5x_reserve", 0, "Pro 5X 三次专用卡补款结果不明，请到 Zovo 核对；系统不会自动重试。")
		} else {
			autoResolve("pro5x_reserve")
		}
		db.WriteAudit("automation", "pro5x_reserve_topup", fmt.Sprintf("operation=%s card=%d amount_minor=%d reserved_minor=%d result=%s", operationID, reuseID, amount, cost, state), "")
		return true
	}
	product, ok := productMap[pro5xReserveProduct]
	if !ok {
		autoAlert("pro5x_reserve", 0, "Pro 5X 指定专卡产品未在上游产品列表中返回，未开卡。")
		return false
	}
	cost, ok := productCost(product, pro5xInitialMinor, true)
	if !ok || product.RechargeFee == nil || *product.RechargeFee != 0 || cost != pro5xInitialMinor+100 {
		autoAlert("pro5x_reserve", 0, "Pro 5X 专卡费率、金额限制或商户限制发生变化，未开卡。")
		return false
	}
	spendable, err := cli.AutomationSpendable(ctx)
	wallet, walletOK := usdMinor(spendable)
	if err != nil || !walletOK || wallet-cost < p.WalletFloor {
		autoAlert("pro5x_reserve", 0, "平台可消费余额不足以准备 $100 的 Pro 5X 专卡并保留安全余额，未开卡。")
		return false
	}
	operationID, err := localRandom()
	if err != nil {
		return false
	}
	operationID = "pro5x-reserve-" + operationID
	dailyBudget := p.DailyBudget
	if dailyBudget > proDailyMaximum {
		dailyBudget = proDailyMaximum
	}
	tx, err := db.DB.Begin()
	if err != nil {
		return false
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT INTO automation_money
		(id,action,card_id,amount_minor,reserved_minor,before_minor,scope,state,created_at)
		SELECT ?,'pro5x_reserve_open',0,?,?,0,?,'inflight',?
		WHERE COALESCE((SELECT SUM(reserved_minor) FROM automation_money WHERE created_at>?),0)+?<=?
		AND NOT EXISTS(SELECT 1 FROM automation_money WHERE state IN ('inflight','unknown','pending'))
		AND (SELECT COUNT(*) FROM automation_money WHERE action IN ('open','pro_open','pro5x_reserve_open') AND created_at>?)<?
		AND EXISTS(SELECT 1 FROM automation_policy WHERE id=1 AND version=?)
		AND EXISTS(SELECT 1 FROM pro5x_card_reserve WHERE id=1 AND state='empty')`,
		operationID, pro5xInitialMinor, cost, scope, now, now-86400, cost, dailyBudget, now-86400, p.DailyOpen, policyVersion)
	if err != nil {
		return false
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		autoAlert("pro5x_reserve", 0, "Pro 5X 专卡受每日预算、开卡次数或待核对资金操作限制，本轮未开卡。")
		return false
	}
	result, err = tx.Exec(`UPDATE pro5x_card_reserve SET card_id=0,money_id=?,state='opening',created_at=?,updated_at=?
		WHERE id=1 AND state='empty'`, operationID, now, now)
	if err != nil {
		return false
	}
	changed, _ = result.RowsAffected()
	if changed != 1 || tx.Commit() != nil {
		return false
	}

	cardID, openErr := cli.AutomationOpen(ctx, pro5xReserveProduct, p.First, p.Last, pro5xInitialMinor, operationID)
	if openErr != nil {
		_, _ = db.DB.Exec("UPDATE automation_money SET state='unknown' WHERE id=? AND state='inflight'", operationID)
		_, _ = db.DB.Exec("UPDATE pro5x_card_reserve SET state='review',updated_at=? WHERE id=1 AND money_id=?", time.Now().Unix(), operationID)
		autoAlert("pro5x_reserve", 0, "Pro 5X 专卡开卡结果不明，请到上游核对；系统不会重复开卡。")
		return true
	}
	tx, err = db.DB.Begin()
	if err != nil {
		_, _ = db.DB.Exec("UPDATE automation_money SET state='unknown',result_card_id=? WHERE id=?", cardID, operationID)
		_, _ = db.DB.Exec("UPDATE pro5x_card_reserve SET card_id=?,state='review',updated_at=? WHERE id=1 AND money_id=?", cardID, time.Now().Unix(), operationID)
		return true
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE automation_money SET state='pending',result_card_id=? WHERE id=? AND state='inflight'", cardID, operationID); err == nil {
		_, err = tx.Exec(`UPDATE pro5x_card_reserve SET card_id=?,state='funding',updated_at=?
			WHERE id=1 AND money_id=? AND state='opening'`, cardID, time.Now().Unix(), operationID)
	}
	if err != nil {
		_ = tx.Rollback()
		_, _ = db.DB.Exec("UPDATE automation_money SET state='unknown',result_card_id=? WHERE id=?", cardID, operationID)
		_, _ = db.DB.Exec("UPDATE pro5x_card_reserve SET card_id=?,state='review',updated_at=? WHERE id=1 AND money_id=?", cardID, time.Now().Unix(), operationID)
		autoAlert("pro5x_reserve", 0, "Pro 5X 专卡已开出但本站记录待核对；系统不会重复开卡。")
		return true
	}
	if err = tx.Commit(); err != nil {
		_, _ = db.DB.Exec("UPDATE automation_money SET state='unknown',result_card_id=? WHERE id=?", cardID, operationID)
		_, _ = db.DB.Exec("UPDATE pro5x_card_reserve SET card_id=?,state='review',updated_at=? WHERE id=1 AND money_id=?", cardID, time.Now().Unix(), operationID)
		autoAlert("pro5x_reserve", 0, "Pro 5X 专卡已开出但本站记录待核对；系统不会重复开卡。")
		return true
	}
	db.WriteAudit("automation", "pro5x_reserve_open", fmt.Sprintf("operation=%s card=%d reserved_minor=%d", operationID, cardID, cost), "")
	autoResolve("pro5x_reserve")
	return true
}

func recoverInterruptedPro5xReserve() {
	reserve, err := loadPro5xReserve()
	if err != nil || reserve.State != "opening" {
		return
	}
	result, _ := db.DB.Exec("UPDATE automation_money SET state='unknown' WHERE id=? AND state='inflight'", reserve.MoneyID)
	changed, _ := result.RowsAffected()
	if changed > 0 {
		_, _ = db.DB.Exec("UPDATE pro5x_card_reserve SET state='review',updated_at=? WHERE id=1 AND state='opening'", time.Now().Unix())
		autoAlert("pro5x_reserve", 0, "服务重启前有 Pro 5X 专卡正在开立，请核对上游；系统不会重复开卡。")
	}
}

func claimPro5xReserve(tx *sql.Tx, localID int64, moneyID, scope string, fee, dailyBudget, now int64) (int64, error) {
	var cardID int64
	err := tx.QueryRow(`SELECT card_id FROM pro5x_card_reserve
		WHERE id=1 AND state='ready' AND card_id>0`).Scan(&cardID)
	if err != nil {
		return 0, err
	}
	result, err := tx.Exec(`UPDATE pro5x_card_reserve
		SET card_id=0,money_id='',state='empty',created_at=0,updated_at=?
		WHERE id=1 AND state='ready' AND card_id=?`, now, cardID)
	if err != nil {
		return 0, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return 0, sql.ErrNoRows
	}
	result, err = tx.Exec(`INSERT INTO automation_money
		(id,action,card_id,amount_minor,reserved_minor,before_minor,scope,state,created_at)
		SELECT ?,'pro5x_pay_fee',?,0,?,0,?,'balance_verified',?
		WHERE COALESCE((SELECT SUM(reserved_minor) FROM automation_money WHERE created_at>?),0)+?<=?`,
		moneyID, cardID, fee, scope, now, now-86400, fee, dailyBudget)
	if err != nil {
		return 0, err
	}
	changed, _ = result.RowsAffected()
	if changed != 1 {
		return 0, sql.ErrNoRows
	}
	if _, err = tx.Exec(`INSERT INTO pro_dedicated_orders(local_id,card_id,money_id,state,created_at,api_fee_minor)
		VALUES(?,?,?,'funding',?,?)`, localID, cardID, moneyID, now, fee); err != nil {
		return 0, err
	}
	// A claimed reserve creates a temporary inventory gap. Wake the money
	// maintainer on its next 30-second scheduler tick instead of waiting for the
	// ordinary five-minute funding interval before preparing the replacement.
	if _, err = tx.Exec("UPDATE automation_runtime SET money_after=0 WHERE id=1"); err != nil {
		return 0, err
	}
	return cardID, nil
}
