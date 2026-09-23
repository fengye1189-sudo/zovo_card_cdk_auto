package handler

import (
	"context"
	"fmt"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

const walletWarningMinor int64 = 10000

var operationsBangkok = time.FixedZone("Asia/Bangkok", 7*60*60)

func initOperationsDigest() error {
	_, err := db.DB.Exec(`CREATE TABLE IF NOT EXISTS operations_wallet_snapshot(
 id INTEGER PRIMARY KEY CHECK(id=1),balance_minor INTEGER NOT NULL,checked_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS operations_wallet_probe(
 id INTEGER PRIMARY KEY CHECK(id=1),consecutive_failures INTEGER NOT NULL DEFAULT 0,last_failure_at INTEGER NOT NULL DEFAULT 0);
INSERT OR IGNORE INTO operations_wallet_probe(id) VALUES(1)`)
	return err
}

func recordWalletQueryResult(ctx context.Context, failed bool) (int64, error) {
	if !failed {
		_, err := db.DB.ExecContext(ctx, "UPDATE operations_wallet_probe SET consecutive_failures=0 WHERE id=1")
		return 0, err
	}
	if _, err := db.DB.ExecContext(ctx, `UPDATE operations_wallet_probe
 SET consecutive_failures=consecutive_failures+1,last_failure_at=? WHERE id=1`, time.Now().Unix()); err != nil {
		return 0, err
	}
	var failures int64
	err := db.DB.QueryRowContext(ctx, "SELECT consecutive_failures FROM operations_wallet_probe WHERE id=1").Scan(&failures)
	return failures, err
}

func checkOperationsWallet(ctx context.Context) {
	cli := cardplatform.New(cardplatform.LoadConfig())
	value, err := cli.AutomationSpendable(ctx)
	balance, ok := usdMinor(value)
	if err != nil || !ok {
		failures, stateErr := recordWalletQueryResult(ctx, true)
		if stateErr == nil {
			operationsSetAlert("wallet-query", failures >= 2, "上游可消费余额连续两次查询失败，无法确认资金是否充足；请留意运行摘要中的余额更新时间。")
		}
		return
	}
	_, _ = recordWalletQueryResult(ctx, false)
	operationsSetAlert("wallet-query", false, "")
	_, err = db.DB.ExecContext(ctx, `INSERT INTO operations_wallet_snapshot VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET balance_minor=excluded.balance_minor,checked_at=excluded.checked_at`, balance, time.Now().Unix())
	if err != nil {
		return
	}
	p, _, err := readAutomationPolicy()
	if err != nil {
		return
	}
	operationsSetAlert("wallet-low", balance < walletWarningMinor, "上游可消费余额低于 $100，请及时补充平台余额。系统不会替你转入资金；每日摘要会列出余额和查询时间。")
	operationsSetAlert("wallet-critical", balance < p.WalletFloor+p.MaxOperation, fmt.Sprintf("上游可消费余额不足 $%.2f，可能无法在保留 $%.2f 安全余额后完成一笔 $%.2f 的资金操作，请及时充值。", float64(p.WalletFloor+p.MaxOperation)/100, float64(p.WalletFloor)/100, float64(p.MaxOperation)/100))
}

func queueOperationsDigest(ctx context.Context, now time.Time) {
	local := now.In(operationsBangkok)
	if local.Hour() < 9 {
		return
	}
	p, v, err := readNotificationConfig()
	if err != nil || !p.Enabled || p.Channel != "telegram" || !validNotification(p) {
		return
	}
	report, err := collectOperationsHealth(ctx, db.DB)
	if err != nil {
		return
	}
	// Healthy operation is intentionally silent. The bot is reserved for
	// actionable conditions, per the owner's notification preference.
	if actionableNotificationAlerts(now.Unix()) == 0 {
		return
	}
	balanceText := "暂未取得，请人工核查"
	var balance, at int64
	if db.DB.QueryRowContext(ctx, "SELECT balance_minor,checked_at FROM operations_wallet_snapshot WHERE id=1").Scan(&balance, &at) == nil {
		balanceText = fmt.Sprintf("$%.2f（查询于 %s）", float64(balance)/100, time.Unix(at, 0).In(operationsBangkok).Format("01-02 15:04"))
		if now.Unix()-at > 1800 {
			balanceText += "，数据已过期"
		}
	}
	backup := "无已校验备份"
	if report.LastBackupSuccess > 0 {
		backup = time.Unix(report.LastBackupSuccess, 0).In(operationsBangkok).Format("01-02 15:04")
	}
	var funding int64
	if err = db.DB.QueryRowContext(ctx, "SELECT COALESCE(SUM(amount_minor),0) FROM automation_money WHERE created_at>=? AND state='balance_verified'", now.Unix()-86400).Scan(&funding); err != nil {
		return
	}
	message := fmt.Sprintf("📋 枫叶兑换站 · 每日运行摘要\n%s（曼谷时间）\n\n自动任务心跳正常：%t\n自动资金操作暂停：%t\n上游可消费余额：%s\n\n过去24小时提交：%d 单，其中已完成 %d 单、失败 %d 单\n当前在途：%d 单，待核对：%d 单\n过去24小时发起且已核查到账的开卡/补款本金：$%.2f（不含手续费）\n候选卡：%d 张／已选 %d 张\n未解除异常：%d 条\n最近本机备份校验：%s\n\n本摘要不是独立离线监测；服务器停机时无法发送。", local.Format("2006-01-02 15:04"), report.HeartbeatOK, report.Paused, balanceText, report.Submitted24h, report.Completed24hCohort, report.Failed24hCohort, report.Outstanding, report.Review, float64(funding)/100, report.CandidateCards, report.SelectedCards, report.ActiveAlerts, backup)
	id := localHash("daily-digest|" + local.Format("2006-01-02"))
	// One daily snapshot across restarts. Requeue only if an unsent message was
	// cancelled by an explicit destination/configuration change.
	_, _ = db.DB.ExecContext(ctx, `INSERT INTO notification_outbox(id,config_version,created_at,updated_at,message) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET config_version=excluded.config_version,message=excluded.message,state='pending',attempts=0,next_run=0,updated_at=excluded.updated_at WHERE notification_outbox.state='cancelled'`, id, v, now.Unix(), now.Unix(), message)
}

func startOperationsDigest(ctx context.Context) error {
	if err := initOperationsDigest(); err != nil {
		return err
	}
	go func() {
		timer := time.NewTimer(20 * time.Second)
		defer timer.Stop()
		var checked time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				if time.Since(checked) >= 15*time.Minute {
					check, cancel := context.WithTimeout(ctx, 25*time.Second)
					checkOperationsWallet(check)
					cancel()
					checked = time.Now()
				}
				check, cancel := context.WithTimeout(ctx, 15*time.Second)
				queueOperationsDigest(check, time.Now())
				cancel()
				timer.Reset(time.Minute)
			}
		}
	}()
	return nil
}
