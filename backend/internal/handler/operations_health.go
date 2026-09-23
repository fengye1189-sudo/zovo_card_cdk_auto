package handler

import (
	"context"
	"database/sql"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

type operationsCard struct {
	ID            int64  `json:"id"`
	Completed     int64  `json:"completed"`
	Outstanding   int64  `json:"outstanding"`
	Remaining     int64  `json:"remaining"`
	Limit         int64  `json:"limit"`
	Kind          string `json:"kind"`
	CooldownUntil int64  `json:"cooldown_until"`
	Phase         string `json:"phase"`
	RetireState   string `json:"retire_state"`
	Declines      int64  `json:"declines"`
	Capped        bool   `json:"capped"`
}

type operationsHealth struct {
	CheckedAt                int64            `json:"checked_at"`
	DatabaseOK               bool             `json:"database_ok"`
	DatabaseBytes            int64            `json:"database_bytes"`
	Heartbeat                int64            `json:"heartbeat"`
	HeartbeatOK              bool             `json:"heartbeat_ok"`
	SyncEnabled              bool             `json:"sync_enabled"`
	Paused                   bool             `json:"paused"`
	ChannelEnabled           bool             `json:"channel_enabled"`
	RestorePending           bool             `json:"restore_pending"`
	Codes                    int64            `json:"codes"`
	Issued24h                int64            `json:"issued_24h"`
	Submitted24h             int64            `json:"submitted_24h"`
	Completed24hCohort       int64            `json:"completed_24h_cohort"`
	Failed24hCohort          int64            `json:"failed_24h_cohort"`
	Completed                int64            `json:"completed"`
	Outstanding              int64            `json:"outstanding"`
	Review                   int64            `json:"review"`
	Overdue                  int64            `json:"overdue"`
	MoneyHolds               int64            `json:"money_holds"`
	SelectedCards            int              `json:"selected_cards"`
	CappedCards              int              `json:"capped_cards"`
	CandidateCards           int              `json:"candidate_cards"`
	PaymentLimit             int              `json:"payment_limit"`
	Cards                    []operationsCard `json:"cards"`
	NotificationsEnabled     bool             `json:"notifications_enabled"`
	NotificationChannel      string           `json:"notification_channel"`
	NotificationsPending     int64            `json:"notifications_pending"`
	NotificationsFailed      int64            `json:"notifications_failed"`
	NotificationsUnknown     int64            `json:"notifications_unknown"`
	LastNotificationAccepted int64            `json:"last_notification_accepted"`
	LastBackupSuccess        int64            `json:"last_backup_success"`
	LastBackupError          string           `json:"last_backup_error"`
	LastHealthCheck          int64            `json:"last_health_check"`
	ActiveAlerts             int64            `json:"active_alerts"`
	Alerts                   []gin.H          `json:"alerts"`
}

// Initialize once after the application's schema. Its loop is independent of
// payment reconciliation, so a stalled automation cycle can still raise an alert.
func InitOperations(ctx context.Context) error {
	if err := initOperationsSchema(db.DB); err != nil {
		return err
	}
	if err := startOperationsDigest(ctx); err != nil {
		return err
	}
	folder, err := operationsBackupFolder(ctx, db.DB)
	if err != nil {
		return err
	}
	manager := &operationsManager{database: db.DB, folder: folder}
	operationsService = manager
	go func() {
		timer := time.NewTimer(15 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				operationsDailyBackup(ctx, manager)
				checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				operationsCheckHealth(checkCtx)
				cancel()
				timer.Reset(time.Minute)
			}
		}
	}()
	return nil
}

func initOperationsSchema(database *sql.DB) error {
	_, err := database.Exec(`CREATE TABLE IF NOT EXISTS operations_runtime (
	 id INTEGER PRIMARY KEY CHECK(id=1), last_health_check INTEGER NOT NULL DEFAULT 0,
	 last_backup_attempt INTEGER NOT NULL DEFAULT 0, last_backup_success INTEGER NOT NULL DEFAULT 0,
	 last_backup_error TEXT NOT NULL DEFAULT ''
	); INSERT OR IGNORE INTO operations_runtime(id) VALUES(1);
	CREATE INDEX IF NOT EXISTS idx_local_cdk_card_status ON local_cdks(card_id,status);
	CREATE INDEX IF NOT EXISTS idx_local_cdk_created ON local_cdks(created_at);`)
	return err
}

func operationsDailyBackup(ctx context.Context, manager *operationsManager) {
	now := time.Now().Unix()
	var lastAttempt int64
	if err := manager.database.QueryRowContext(ctx, "SELECT last_backup_attempt FROM operations_runtime WHERE id=1").Scan(&lastAttempt); err != nil {
		return
	}
	if now-lastAttempt < 3600 {
		return
	}
	items, err := manager.list()
	if err == nil {
		for _, item := range items {
			if item.Kind == "daily" && now-item.CreatedAt < 86400 {
				return
			}
		}
	}
	if _, err := manager.database.ExecContext(ctx, "UPDATE operations_runtime SET last_backup_attempt=? WHERE id=1", now); err != nil {
		return
	}
	if _, err := manager.create(ctx, "daily"); err != nil {
		if err != errOperationsBusy {
			operationsBackupFailed()
		}
		return
	}
	operationsSetAlert("backup-failed", false, "")
	operationsSetAlert("backup-stale", false, "")
}

func collectOperationsHealth(ctx context.Context, database *sql.DB) (operationsHealth, error) {
	report := operationsHealth{CheckedAt: time.Now().Unix(), Cards: []operationsCard{}, Alerts: []gin.H{}}
	if err := database.PingContext(ctx); err != nil {
		return report, err
	}
	report.DatabaseOK = true
	var pages, pageSize int64
	if err := database.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return report, err
	}
	if err := database.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return report, err
	}
	report.DatabaseBytes = pages * pageSize
	if err := database.QueryRowContext(ctx, "SELECT heartbeat FROM automation_runtime WHERE id=1").Scan(&report.Heartbeat); err != nil {
		return report, err
	}
	report.HeartbeatOK = report.Heartbeat > 0 && report.CheckedAt-report.Heartbeat < 600
	policy, _, err := readAutomationPolicy()
	if err != nil {
		return report, err
	}
	report.SyncEnabled, report.Paused = policy.Sync, policy.Paused
	settings := readLocalSettings()
	report.ChannelEnabled, report.PaymentLimit = settings.Enabled, 0
	if database == db.DB {
		if _, _, err := localPoolCards(settings, report.CheckedAt); err != nil {
			return report, err
		}
	}
	if err := database.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM site_settings WHERE key='operations_restore_pending' AND value<>'')").Scan(&report.RestorePending); err != nil {
		return report, err
	}
	queries := []struct {
		sql    string
		args   []interface{}
		target *int64
	}{
		{"SELECT COUNT(*) FROM local_cdks", nil, &report.Codes},
		{"SELECT COUNT(*) FROM local_cdks WHERE created_at>=?", []interface{}{report.CheckedAt - 86400}, &report.Issued24h},
		{"SELECT COUNT(*) FROM local_cdks WHERE status='consumed'", nil, &report.Completed},
		{"SELECT COUNT(*) FROM local_cdks WHERE status IN ('reserved','review')", nil, &report.Outstanding},
		{"SELECT COUNT(*) FROM local_cdks WHERE status='review'", nil, &report.Review},
		{"SELECT COUNT(*) FROM local_cdks c JOIN automation_watch w ON w.local_id=c.id WHERE c.status IN ('reserved','review') AND w.first_seen>0 AND w.first_seen<?", []interface{}{report.CheckedAt - 1800}, &report.Overdue},
		{"SELECT COUNT(*) FROM local_cdks c JOIN automation_watch w ON w.local_id=c.id WHERE w.first_seen>=?", []interface{}{report.CheckedAt - 86400}, &report.Submitted24h},
		{"SELECT COUNT(*) FROM local_cdks c JOIN automation_watch w ON w.local_id=c.id WHERE w.first_seen>=? AND c.status='consumed'", []interface{}{report.CheckedAt - 86400}, &report.Completed24hCohort},
		{"SELECT COUNT(*) FROM local_cdks c JOIN automation_watch w ON w.local_id=c.id WHERE w.first_seen>=? AND c.status='failed'", []interface{}{report.CheckedAt - 86400}, &report.Failed24hCohort},
		{"SELECT COUNT(*) FROM automation_money WHERE state IN ('pending','inflight','unknown')", nil, &report.MoneyHolds},
		{"SELECT COUNT(*) FROM automation_alerts WHERE resolved=0", nil, &report.ActiveAlerts},
		{"SELECT COUNT(*) FROM notification_outbox WHERE state IN ('pending','retry','sending') AND config_version=(SELECT version FROM notification_config WHERE id=1)", nil, &report.NotificationsPending},
		{"SELECT COUNT(*) FROM notification_outbox WHERE state='failed' AND updated_at>=? AND config_version=(SELECT version FROM notification_config WHERE id=1)", []interface{}{report.CheckedAt - 86400}, &report.NotificationsFailed},
		{"SELECT COUNT(*) FROM notification_outbox WHERE state='unknown' AND updated_at>=? AND config_version=(SELECT version FROM notification_config WHERE id=1)", []interface{}{report.CheckedAt - 86400}, &report.NotificationsUnknown},
		{"SELECT COALESCE(MAX(updated_at),0) FROM notification_outbox WHERE state='accepted' AND config_version=(SELECT version FROM notification_config WHERE id=1)", nil, &report.LastNotificationAccepted},
	}
	for _, query := range queries {
		if err := database.QueryRowContext(ctx, query.sql, query.args...).Scan(query.target); err != nil {
			return report, err
		}
	}
	notification, _, err := readNotificationConfig()
	if err != nil {
		return report, err
	}
	report.NotificationsEnabled = notification.Enabled && validNotification(notification)
	report.NotificationChannel = notification.Channel
	if err := database.QueryRowContext(ctx, "SELECT last_backup_success,last_backup_error,last_health_check FROM operations_runtime WHERE id=1").Scan(&report.LastBackupSuccess, &report.LastBackupError, &report.LastHealthCheck); err != nil {
		return report, err
	}
	ids := localCardIDs(settings)
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
	}
	dedicatedRows, err := database.QueryContext(ctx, `SELECT p.card_id FROM pro_dedicated_orders p
	 JOIN local_cdks c ON c.id=p.local_id WHERE p.card_id>0 AND p.state IN ('submitted','completed')
	 AND c.plan IN ('pro_5x','pro_20x') AND c.status='consumed' ORDER BY p.local_id`)
	if err != nil {
		return report, err
	}
	for dedicatedRows.Next() {
		var id int64
		if err = dedicatedRows.Scan(&id); err != nil {
			dedicatedRows.Close()
			return report, err
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if err = dedicatedRows.Close(); err != nil {
		return report, err
	}
	report.Cards = make([]operationsCard, 0, len(ids))
	var lifecycleTable int
	if err = database.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='automation_card_lifecycle'").Scan(&lifecycleTable); err != nil {
		return report, err
	}
	for _, id := range ids {
		item := operationsCard{ID: id, RetireState: "active"}
		if err := database.QueryRowContext(ctx, "SELECT COALESCE(SUM(CASE WHEN status IN ('reserved','review') THEN 1 ELSE 0 END),0) FROM local_cdks WHERE card_id=?", id).Scan(&item.Outstanding); err != nil {
			return report, err
		}
		err := database.QueryRowContext(ctx, "SELECT card_kind,success_limit,success_count,cooldown_until FROM local_card_cycles WHERE card_id=?", id).Scan(&item.Kind, &item.Limit, &item.Completed, &item.CooldownUntil)
		if err != nil && err != sql.ErrNoRows {
			return report, err
		}
		if lifecycleTable > 0 {
			err = database.QueryRowContext(ctx, "SELECT phase,retire_state,decline_count FROM automation_card_lifecycle WHERE card_id=?", id).Scan(&item.Phase, &item.RetireState, &item.Declines)
			if err != nil && err != sql.ErrNoRows {
				return report, err
			}
		}
		item.Limit = 0
		item.Remaining = -1
		item.CooldownUntil = 0
		item.Capped = item.RetireState != "active"
		if item.Capped {
			report.CappedCards++
		} else if item.Outstanding == 0 {
			report.CandidateCards++
		}
		report.Cards = append(report.Cards, item)
	}
	report.SelectedCards = len(report.Cards)
	rows, err := database.QueryContext(ctx, "SELECT alert_key,local_id,message,updated_at FROM automation_alerts WHERE resolved=0 ORDER BY updated_at DESC LIMIT 100")
	if err != nil {
		return report, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, message string
		var id, updated int64
		if err := rows.Scan(&key, &id, &message, &updated); err != nil {
			return report, err
		}
		report.Alerts = append(report.Alerts, gin.H{"key": key, "local_id": id, "message": message, "updated_at": updated})
	}
	return report, rows.Err()
}

// Alert keys and messages stay stable while the condition remains unchanged.
// Existing notification_outbox de-duplicates the same alert, avoiding minute spam.
func operationsSetAlert(key string, active bool, message string) {
	key = "operations:" + key
	if !active {
		autoResolve(key)
		return
	}
	var previous string
	var resolved int
	if err := db.DB.QueryRow("SELECT message,resolved FROM automation_alerts WHERE alert_key=?", key).Scan(&previous, &resolved); err == nil && previous == message && resolved == 0 {
		return
	}
	autoAlert(key, 0, message)
}

func operationsCheckHealth(ctx context.Context) {
	report, err := collectOperationsHealth(ctx, db.DB)
	if err != nil {
		operationsSetAlert("monitor", true, "运维检查暂时无法完整读取数据库，请检查服务器；未执行任何付款。")
		return
	}
	_, _ = db.DB.ExecContext(ctx, "UPDATE operations_runtime SET last_health_check=? WHERE id=1", report.CheckedAt)
	operationsSetAlert("monitor", false, "")
	operationsSetAlert("heartbeat", !report.HeartbeatOK, "订单自动化超过 10 分钟没有心跳，请检查服务器后台任务。")
	operationsSetAlert("order-backlog", report.Overdue > 0, "存在超过 30 分钟仍未完成的订单，请在售后工作台核对。")
	operationsSetAlert("notification-delivery", report.NotificationsFailed+report.NotificationsUnknown > 0, "最近 24 小时存在发送失败或结果不明的通知，请检查通知配置和发送记录。")
	operationsSetAlert("card-pool", report.ChannelEnabled && report.CandidateCards < 2, "支付卡池的本地候选卡少于 2 张，请检查卡片使用次数、在途订单及自动开卡规则。")
	operationsSetAlert("backup-stale", report.LastBackupSuccess == 0 || report.CheckedAt-report.LastBackupSuccess > 90000, "超过 25 小时没有已校验的数据库备份，请在运维中心检查并创建备份。")
	operationsSetAlert("backup-size", report.DatabaseBytes > operationsMaxSnapshotBytes, "数据库已超过当前在线备份大小上限，请扩展备份存储并调整上限。")
	operationsSetAlert("restore-pending", report.RestorePending, "数据库已从备份恢复，仍需核对备份之后的付款、卡片次数和卡密状态；核对完成前保持充值与自动资金操作暂停。")
}

func AdminOperationsHealth(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	report, err := collectOperationsHealth(ctx, db.DB)
	if err != nil {
		localError(c, 503, "运行状态暂时无法完整读取，请稍后刷新")
		return
	}
	c.JSON(200, report)
}
