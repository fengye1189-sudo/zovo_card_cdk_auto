package handler

import (
	"context"
	"encoding/json"
	"github.com/tuzi/cdk-recharge-system/internal/db"
	"strings"
	"testing"
	"time"
)

func TestDailyDigestTimeDedupAndNoSecrets(t *testing.T) {
	newOperationsFixture(t)
	if err := initOperationsDigest(); err != nil {
		t.Fatal(err)
	}
	p := notificationConfig{Enabled: true, Channel: "telegram", Recipient: "1234", Secret: "123456:abcdefghijklmnopqrstuvwxyz"}
	raw, _ := json.Marshal(p)
	db.DB.Exec("UPDATE notification_config SET value=?", string(raw))
	now := time.Date(2026, 9, 11, 8, 59, 0, 0, operationsBangkok)
	queueOperationsDigest(context.Background(), now)
	var count int
	db.DB.QueryRow("SELECT COUNT(*) FROM notification_outbox").Scan(&count)
	if count != 0 {
		t.Fatal("before nine")
	}
	db.DB.Exec("INSERT INTO operations_wallet_snapshot VALUES(1,12345,?)", now.Unix())
	now = now.Add(time.Minute)
	queueOperationsDigest(context.Background(), now)
	db.DB.QueryRow("SELECT COUNT(*) FROM notification_outbox").Scan(&count)
	if count != 0 {
		t.Fatal("healthy digest must stay silent")
	}
	autoAlert("actionable-problem", 0, "需要处理的异常")
	queueOperationsDigest(context.Background(), now)
	queueOperationsDigest(context.Background(), now.Add(time.Hour))
	var message string
	db.DB.QueryRow("SELECT COUNT(*),MAX(message) FROM notification_outbox").Scan(&count, &message)
	if count != 1 || !strings.Contains(message, "$123.45") || strings.Contains(message, p.Secret) {
		t.Fatal("bad digest", count)
	}
	queueOperationsDigest(context.Background(), now.Add(24*time.Hour))
	db.DB.QueryRow("SELECT COUNT(*) FROM notification_outbox").Scan(&count)
	if count != 2 {
		t.Fatal("next day missing")
	}
}

func TestWalletMonitorUsesReadOnlyBalance(t *testing.T) {
	a := newAutoFixture(t)
	_ = a
	if err := initOperationsDigest(); err != nil {
		t.Fatal(err)
	}
	p := moneyPolicy()
	putAutoPolicy(t, p)
	checkOperationsWallet(context.Background())
	var balance int64
	if err := db.DB.QueryRow("SELECT balance_minor FROM operations_wallet_snapshot").Scan(&balance); err != nil || balance != 10000 {
		t.Fatal(balance, err)
	}
	var active int
	db.DB.QueryRow("SELECT COUNT(*) FROM automation_alerts WHERE resolved=0").Scan(&active)
	if active != 0 || a.money.Load() != 0 {
		t.Fatal("wallet monitor must not fund or alert at $100")
	}
}

func TestWalletQueryAlertRequiresTwoConsecutiveFailures(t *testing.T) {
	newOperationsFixture(t)
	if err := initOperationsDigest(); err != nil {
		t.Fatal(err)
	}
	operationsSetAlert("wallet-query", true, "legacy single-failure alert")
	ctx := context.Background()
	first, err := recordWalletQueryResult(ctx, true)
	if err != nil || first != 1 {
		t.Fatal(first, err)
	}
	operationsSetAlert("wallet-query", first >= 2, "consecutive failure")
	var active int
	db.DB.QueryRow("SELECT COUNT(*) FROM automation_alerts WHERE alert_key='operations:wallet-query' AND resolved=0").Scan(&active)
	if active != 0 {
		t.Fatal("first failure must stay silent")
	}
	second, err := recordWalletQueryResult(ctx, true)
	if err != nil || second != 2 {
		t.Fatal(second, err)
	}
	operationsSetAlert("wallet-query", second >= 2, "consecutive failure")
	db.DB.QueryRow("SELECT COUNT(*) FROM automation_alerts WHERE alert_key='operations:wallet-query' AND resolved=0").Scan(&active)
	if active != 1 {
		t.Fatal("second consecutive failure must alert")
	}
	if _, err = recordWalletQueryResult(ctx, false); err != nil {
		t.Fatal(err)
	}
	operationsSetAlert("wallet-query", false, "")
	var failures int64
	db.DB.QueryRow("SELECT consecutive_failures FROM operations_wallet_probe WHERE id=1").Scan(&failures)
	db.DB.QueryRow("SELECT COUNT(*) FROM automation_alerts WHERE alert_key='operations:wallet-query' AND resolved=0").Scan(&active)
	if failures != 0 || active != 0 {
		t.Fatal("successful query must reset the sequence and resolve the alert")
	}
}
