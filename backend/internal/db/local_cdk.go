package db

// Local codes are independent of cardplatform_cdk_codes. Only a hash is retained;
// the full code is returned once on issuance, never logged or stored in plaintext.
func InitLocalCDK() error {
	_, err := DB.Exec(`CREATE TABLE IF NOT EXISTS local_cdks (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 code_hash TEXT UNIQUE NOT NULL, prefix TEXT NOT NULL,
 plan TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'unused',
 expires_at INTEGER NOT NULL, created_at INTEGER NOT NULL,
 token_hash TEXT NOT NULL DEFAULT '', device_hash TEXT NOT NULL DEFAULT '',
 token_expires INTEGER NOT NULL DEFAULT 0,
 preflight_hash TEXT NOT NULL DEFAULT '', credential_hash TEXT NOT NULL DEFAULT '',
 preflight_expires INTEGER NOT NULL DEFAULT 0,
 card_id INTEGER NOT NULL DEFAULT 0, pricing_version INTEGER NOT NULL DEFAULT 0,
 request_id TEXT NOT NULL DEFAULT '', upstream_id INTEGER NOT NULL DEFAULT 0,
 email TEXT NOT NULL DEFAULT '', message TEXT NOT NULL DEFAULT '',
 batch_id TEXT NOT NULL DEFAULT '', last_checked INTEGER NOT NULL DEFAULT 0,
 activated_at INTEGER NOT NULL DEFAULT 0,
 subscription_expires_at INTEGER NOT NULL DEFAULT 0,
 upgrade_type TEXT NOT NULL DEFAULT '', expiry_estimated INTEGER NOT NULL DEFAULT 0,
 -- Written only when the direct upstream order check has authoritatively
 -- confirmed completion. A generic historical consumed state is not enough
 -- to authorize a marketplace invoice lookup.
 upstream_completion_verified_at INTEGER NOT NULL DEFAULT 0,
 upstream_completion_source TEXT NOT NULL DEFAULT ''
 ); CREATE INDEX IF NOT EXISTS idx_local_cdk_token ON local_cdks(token_hash);
 CREATE TABLE IF NOT EXISTS local_card_selections (
  local_id INTEGER PRIMARY KEY, card_id INTEGER NOT NULL, selected_at INTEGER NOT NULL
 );
 CREATE INDEX IF NOT EXISTS idx_local_card_selections_card ON local_card_selections(card_id,selected_at);
 -- The marketplace can bind an irreversible code hash to its opaque paid order
 -- ID. No purchaser contact details, plaintext codes, or account credentials
 -- belong in this operational database.
 CREATE TABLE IF NOT EXISTS marketplace_local_cdk_bindings (
  local_id INTEGER PRIMARY KEY, code_hash TEXT UNIQUE NOT NULL,
  marketplace_order_id TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL
 );
 CREATE INDEX IF NOT EXISTS idx_marketplace_local_cdk_order
 ON marketplace_local_cdk_bindings(marketplace_order_id);
 -- Keep the one-order-to-one-code invariant when a database was initialized
 -- by an earlier build that did not yet have the column-level UNIQUE clause.
 CREATE UNIQUE INDEX IF NOT EXISTS idx_marketplace_local_cdk_order_unique
 ON marketplace_local_cdk_bindings(marketplace_order_id);
 -- A durable outbound completion event is committed with the authoritative
 -- consumed transition and retried by the automation cycle until acknowledged.
 CREATE TABLE IF NOT EXISTS marketplace_completion_outbox (
  event_id TEXT PRIMARY KEY, local_id INTEGER UNIQUE NOT NULL, code_hash TEXT NOT NULL,
  marketplace_order_id TEXT NOT NULL, plan TEXT NOT NULL, completed_at INTEGER NOT NULL,
  state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','sending','sent')),
  attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at INTEGER NOT NULL,
  lease_until INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, sent_at INTEGER NOT NULL DEFAULT 0
 );
 CREATE INDEX IF NOT EXISTS idx_marketplace_completion_outbox_due
 ON marketplace_completion_outbox(state,next_attempt_at,lease_until);
 -- A confirmed failure is terminal. Keeping this invariant in SQLite prevents
 -- a future handler, admin action, or recovery job from returning a failed
 -- code to inventory.
 CREATE TRIGGER IF NOT EXISTS trg_local_cdk_failed_terminal
 BEFORE UPDATE OF status ON local_cdks
 WHEN OLD.status='failed' AND NEW.status<>'failed'
 BEGIN SELECT RAISE(ABORT,'failed CDK is permanently locked'); END;
 UPDATE local_cdks
 SET message='升级未完成，该卡密已永久锁定，不能再次兑换。请点击联系客服处理。'
 WHERE status='failed';
 CREATE TABLE IF NOT EXISTS local_code_reserve (
 id INTEGER PRIMARY KEY AUTOINCREMENT,product_id TEXT NOT NULL,code_hash TEXT UNIQUE NOT NULL,
 encrypted_code BLOB NOT NULL,created_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS idx_local_code_reserve_product ON local_code_reserve(product_id);
 CREATE TABLE IF NOT EXISTS local_code_replacements (
 old_id INTEGER PRIMARY KEY,new_id INTEGER UNIQUE NOT NULL,request_hash TEXT NOT NULL,
 device_hash TEXT NOT NULL,product_id TEXT NOT NULL,encrypted_code BLOB NOT NULL,created_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS pro_dedicated_orders (
 local_id INTEGER PRIMARY KEY,card_id INTEGER UNIQUE,money_id TEXT UNIQUE NOT NULL,
 state TEXT NOT NULL,created_at INTEGER NOT NULL,api_fee_minor INTEGER NOT NULL DEFAULT 0
 );
 CREATE TABLE IF NOT EXISTS pro5x_card_reserve (
  id INTEGER PRIMARY KEY CHECK(id=1), card_id INTEGER UNIQUE NOT NULL DEFAULT 0,
  money_id TEXT UNIQUE NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'empty' CHECK(state IN ('empty','opening','funding','ready','review')),
  created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0
 );
 INSERT OR IGNORE INTO pro5x_card_reserve(id) VALUES(1);
 CREATE TABLE IF NOT EXISTS pro5x_card_policy (
  card_id INTEGER PRIMARY KEY, completed_uses INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
 );
 CREATE TABLE IF NOT EXISTS local_card_cycles (
 card_id INTEGER PRIMARY KEY,
 card_kind TEXT NOT NULL CHECK(card_kind IN ('ordinary','pro')),
 success_limit INTEGER NOT NULL,
 success_count INTEGER NOT NULL DEFAULT 0,
 cycle_started_at INTEGER NOT NULL,
 cooldown_until INTEGER NOT NULL DEFAULT 0,
 updated_at INTEGER NOT NULL
 );
 CREATE INDEX IF NOT EXISTS idx_local_card_cycles_cooldown ON local_card_cycles(cooldown_until);
 CREATE TABLE IF NOT EXISTS local_card_cycle_migrations (name TEXT PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS automation_card_lifecycle (
  card_id INTEGER PRIMARY KEY,
  product_code TEXT NOT NULL DEFAULT '', bin TEXT NOT NULL DEFAULT '',
  phase TEXT NOT NULL DEFAULT 'primary', decline_count INTEGER NOT NULL DEFAULT 0,
  retire_state TEXT NOT NULL DEFAULT 'active', retire_reason TEXT NOT NULL DEFAULT '',
  retire_attempts INTEGER NOT NULL DEFAULT 0, before_minor INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, closed_at INTEGER NOT NULL DEFAULT 0
 );
 CREATE INDEX IF NOT EXISTS idx_automation_card_retire ON automation_card_lifecycle(retire_state,updated_at);
 CREATE TABLE IF NOT EXISTS automation_card_declines (
  local_id INTEGER PRIMARY KEY, card_id INTEGER NOT NULL, status TEXT NOT NULL,
  email_norm TEXT NOT NULL DEFAULT '',
  recorded_at INTEGER NOT NULL
 );
 CREATE INDEX IF NOT EXISTS idx_automation_card_declines_card ON automation_card_declines(card_id);
 CREATE TABLE IF NOT EXISTS automation_card_outcomes (
  local_id INTEGER PRIMARY KEY, card_id INTEGER NOT NULL, outcome TEXT NOT NULL,
  occurred_at INTEGER NOT NULL
 );
 CREATE INDEX IF NOT EXISTS idx_automation_card_outcomes_time ON automation_card_outcomes(occurred_at);
 CREATE TABLE IF NOT EXISTS automation_product_daily (
  day TEXT NOT NULL, product_code TEXT NOT NULL, bin TEXT NOT NULL DEFAULT '',
  successes INTEGER NOT NULL DEFAULT 0, declines INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL,
  PRIMARY KEY(day,product_code,bin)
 );
 CREATE TABLE IF NOT EXISTS automation_product_catalog (
  product_code TEXT PRIMARY KEY, bin TEXT NOT NULL DEFAULT '',
  card_type TEXT NOT NULL DEFAULT '', first_seen INTEGER NOT NULL,
  last_seen INTEGER NOT NULL
 );
 CREATE INDEX IF NOT EXISTS idx_automation_product_catalog_type
 ON automation_product_catalog(card_type,bin);
 CREATE TABLE IF NOT EXISTS automation_product_selections (
  operation_id TEXT PRIMARY KEY, product_code TEXT NOT NULL, bin TEXT NOT NULL DEFAULT '',
  selection_mode TEXT NOT NULL CHECK(selection_mode IN ('best','explore','fixed')),
  selected_at INTEGER NOT NULL
 );
 CREATE INDEX IF NOT EXISTS idx_automation_product_selections_head
 ON automation_product_selections(bin,product_code,selected_at);
 BEGIN IMMEDIATE;
 -- Preserve the original cooldown start, including Pro cards whose updated_at
 -- may have been refreshed by monitoring. Run exactly once across restarts.
 UPDATE local_card_cycles SET cooldown_until=cooldown_until-23*86400
 WHERE cooldown_until>0 AND NOT EXISTS(
 SELECT 1 FROM local_card_cycle_migrations WHERE name='cooldown_7_days');
 INSERT OR IGNORE INTO local_card_cycle_migrations(name) VALUES('cooldown_7_days');
 -- Shorten an already-running seven-day cooldown without restarting it. The
 -- original trigger time is preserved and this migration runs exactly once.
 UPDATE local_card_cycles SET cooldown_until=cooldown_until-5*86400
 WHERE cooldown_until>0 AND NOT EXISTS(
 SELECT 1 FROM local_card_cycle_migrations WHERE name='cooldown_2_days_final_two');
 INSERT OR IGNORE INTO local_card_cycle_migrations(name) VALUES('cooldown_2_days_final_two');
 COMMIT;
 BEGIN IMMEDIATE;
 -- Owner policy: eligible cards remain in the balanced random pool without a
 -- payment-count cap or cooldown. Keep success_count only as history.
 UPDATE local_card_cycles SET success_limit=2147483647,cooldown_until=0;
 UPDATE automation_card_lifecycle SET phase='primary',retire_state='active',retire_reason='',updated_at=strftime('%s','now')
 WHERE retire_state='queued' AND retire_reason='final_two_successes';
 INSERT OR IGNORE INTO local_card_cycle_migrations(name) VALUES('balanced_random_no_cooldown');
 COMMIT;
 UPDATE pro_dedicated_orders SET state='completed'
 WHERE state='submitted' AND EXISTS(
  SELECT 1 FROM local_cdks c WHERE c.id=pro_dedicated_orders.local_id AND c.plan IN ('pro_5x','pro_20x') AND c.status='consumed'
 );
 INSERT OR IGNORE INTO local_card_cycles(card_id,card_kind,success_limit,success_count,cycle_started_at,cooldown_until,updated_at)
 SELECT p.card_id,CASE WHEN c.plan='pro_5x' THEN 'ordinary' ELSE 'pro' END,
  2147483647,
  0,strftime('%s','now'),0,strftime('%s','now')
 FROM pro_dedicated_orders p JOIN local_cdks c ON c.id=p.local_id
 WHERE p.card_id>0 AND p.state='completed' AND c.plan IN ('pro_5x','pro_20x') AND c.status='consumed';
 UPDATE local_card_cycles SET
  card_kind='ordinary',
  success_limit=2147483647,
  cooldown_until=0,
  updated_at=strftime('%s','now')
 WHERE card_kind='pro' AND card_id IN (
  SELECT p.card_id FROM pro_dedicated_orders p JOIN local_cdks c ON c.id=p.local_id
  WHERE p.state='completed' AND c.plan='pro_5x' AND c.status='consumed'
 ) AND NOT EXISTS(
  SELECT 1 FROM local_card_cycle_migrations WHERE name='pro5x_to_plus_rules'
 );
 INSERT OR IGNORE INTO local_card_cycle_migrations(name) VALUES('pro5x_to_plus_rules');
 INSERT OR IGNORE INTO automation_card_lifecycle(card_id,created_at,updated_at)
 SELECT card_id,strftime('%s','now'),strftime('%s','now') FROM local_card_cycles WHERE card_id>0;
 INSERT OR IGNORE INTO automation_card_lifecycle(card_id,created_at,updated_at)
 SELECT DISTINCT card_id,strftime('%s','now'),strftime('%s','now') FROM local_cdks WHERE card_id>0;
 CREATE TABLE IF NOT EXISTS direct_admin_actions (
 action_key TEXT PRIMARY KEY, attempted_at INTEGER NOT NULL, state TEXT NOT NULL
 );
 CREATE TABLE IF NOT EXISTS automation_policy (id INTEGER PRIMARY KEY CHECK(id=1), value TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1);
 INSERT OR IGNORE INTO automation_policy(id,value) VALUES(1,'{"sync_enabled":true}');
 CREATE TABLE IF NOT EXISTS automation_watch (
 local_id INTEGER PRIMARY KEY, next_check INTEGER NOT NULL DEFAULT 0,
 first_seen INTEGER NOT NULL DEFAULT 0, checked_at INTEGER NOT NULL DEFAULT 0,
 status TEXT NOT NULL DEFAULT '', renewal TEXT NOT NULL DEFAULT '',
 failures INTEGER NOT NULL DEFAULT 0, renewal_attempts INTEGER NOT NULL DEFAULT 0,
 renewal_after INTEGER NOT NULL DEFAULT 0, scan_page INTEGER NOT NULL DEFAULT 1,
 snapshot TEXT NOT NULL DEFAULT '{}'
 );
 CREATE INDEX IF NOT EXISTS idx_automation_watch_due ON automation_watch(next_check);
 -- Seed balancing history for orders created before fair-random selection was
 -- introduced. first_seen is closest to the real submission time; the other
 -- values are conservative fallbacks for older rows.
 INSERT OR IGNORE INTO local_card_selections(local_id,card_id,selected_at)
 SELECT c.id,c.card_id,COALESCE(NULLIF(w.first_seen,0),NULLIF(c.last_checked,0),c.created_at)
 FROM local_cdks c LEFT JOIN automation_watch w ON w.local_id=c.id
 WHERE c.card_id>0 AND c.request_id<>'';
 -- Import only authoritative historical outcomes. Ambiguous failed/review
 -- states intentionally do not count as a decline.
 INSERT OR IGNORE INTO automation_card_declines(local_id,card_id,status,recorded_at)
 SELECT w.local_id,c.card_id,w.status,
  CASE WHEN w.checked_at>0 THEN w.checked_at ELSE c.created_at END
 FROM automation_watch w JOIN local_cdks c ON c.id=w.local_id
 WHERE c.card_id>0 AND w.status IN ('declined','failed_precharge');
 INSERT OR IGNORE INTO automation_card_outcomes(local_id,card_id,outcome,occurred_at)
 SELECT w.local_id,c.card_id,
  CASE WHEN w.status='completed' THEN 'success' ELSE 'decline' END,
  CASE WHEN w.checked_at>0 THEN w.checked_at ELSE c.created_at END
 FROM automation_watch w JOIN local_cdks c ON c.id=w.local_id
 WHERE c.card_id>0 AND w.status IN ('completed','declined','failed_precharge');
 UPDATE automation_card_lifecycle SET decline_count=(
  SELECT COUNT(*) FROM automation_card_declines d
  WHERE d.card_id=automation_card_lifecycle.card_id
 );
 CREATE TABLE IF NOT EXISTS automation_alerts (
 alert_key TEXT PRIMARY KEY, local_id INTEGER NOT NULL DEFAULT 0,
 message TEXT NOT NULL, updated_at INTEGER NOT NULL, resolved INTEGER NOT NULL DEFAULT 0
 );
 CREATE TABLE IF NOT EXISTS automation_money (
 id TEXT PRIMARY KEY, action TEXT NOT NULL, card_id INTEGER NOT NULL DEFAULT 0, scope TEXT NOT NULL DEFAULT '',
 amount_minor INTEGER NOT NULL, reserved_minor INTEGER NOT NULL, before_minor INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL, created_at INTEGER NOT NULL, result_card_id INTEGER NOT NULL DEFAULT 0
 );
 CREATE INDEX IF NOT EXISTS idx_automation_money_created ON automation_money(created_at);
 CREATE TABLE IF NOT EXISTS automation_money_evidence (
  operation_id TEXT PRIMARY KEY, scope TEXT NOT NULL, source TEXT NOT NULL,
  upstream_id INTEGER NOT NULL, confirmed_at INTEGER NOT NULL,
  UNIQUE(scope,source,upstream_id)
 );
 CREATE TABLE IF NOT EXISTS automation_runtime (id INTEGER PRIMARY KEY CHECK(id=1), heartbeat INTEGER NOT NULL DEFAULT 0, money_after INTEGER NOT NULL DEFAULT 0);
 INSERT OR IGNORE INTO automation_runtime(id) VALUES(1);
 CREATE TABLE IF NOT EXISTS automation_card_remark_runtime (
  id INTEGER PRIMARY KEY CHECK(id=1), next_run INTEGER NOT NULL DEFAULT 0,
  last_success INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT ''
 );
 INSERT OR IGNORE INTO automation_card_remark_runtime(id) VALUES(1);
 CREATE TABLE IF NOT EXISTS finance_records (
 scope TEXT NOT NULL, source TEXT NOT NULL, card_key INTEGER NOT NULL,
 upstream_id INTEGER NOT NULL, card_id INTEGER NOT NULL, ref_id INTEGER NOT NULL,
 kind TEXT NOT NULL, payload TEXT NOT NULL, check_state TEXT NOT NULL,
 imported_at INTEGER NOT NULL, PRIMARY KEY(scope,source,card_key,upstream_id)
 );
 CREATE INDEX IF NOT EXISTS idx_finance_recent ON finance_records(scope,imported_at DESC);
 CREATE TABLE IF NOT EXISTS finance_sync (
 scope TEXT PRIMARY KEY, next_page INTEGER NOT NULL DEFAULT 1, next_run INTEGER NOT NULL DEFAULT 0,
 last_ok INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '', total INTEGER NOT NULL DEFAULT 0,
 card_cursor INTEGER NOT NULL DEFAULT 0
 );
 CREATE TABLE IF NOT EXISTS finance_reviews (
 operation_id TEXT PRIMARY KEY, scope TEXT NOT NULL, wallet_id INTEGER NOT NULL,
 note TEXT NOT NULL, actor TEXT NOT NULL, reviewed_at INTEGER NOT NULL,
 UNIQUE(scope,wallet_id)
 );
 CREATE TABLE IF NOT EXISTS finance_cards(scope TEXT NOT NULL,card_id INTEGER NOT NULL,PRIMARY KEY(scope,card_id));
 CREATE TABLE IF NOT EXISTS finance_transactions (
 scope TEXT NOT NULL, card_id INTEGER NOT NULL, auth_id TEXT NOT NULL, kind TEXT NOT NULL,
 payload TEXT NOT NULL, imported_at INTEGER NOT NULL, PRIMARY KEY(scope,card_id,auth_id,kind)
 );
 CREATE INDEX IF NOT EXISTS idx_finance_transactions_recent ON finance_transactions(scope,imported_at DESC);
 CREATE TABLE IF NOT EXISTS finance_transaction_scan(scope TEXT NOT NULL,card_id INTEGER NOT NULL,next_page INTEGER NOT NULL DEFAULT 2,PRIMARY KEY(scope,card_id));
 CREATE TABLE IF NOT EXISTS notification_config (
 id INTEGER PRIMARY KEY CHECK(id=1), value TEXT NOT NULL DEFAULT '{}', version INTEGER NOT NULL DEFAULT 1
 );
 INSERT OR IGNORE INTO notification_config(id) VALUES(1);
 CREATE TABLE IF NOT EXISTS notification_outbox (
 id TEXT PRIMARY KEY, config_version INTEGER NOT NULL, state TEXT NOT NULL DEFAULT 'pending',
 attempts INTEGER NOT NULL DEFAULT 0, next_run INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, message TEXT NOT NULL,
 error TEXT NOT NULL DEFAULT ''
 );
 CREATE INDEX IF NOT EXISTS idx_notification_due ON notification_outbox(state,next_run);
 CREATE TABLE IF NOT EXISTS notification_alert_state (
 alert_key TEXT PRIMARY KEY, message TEXT NOT NULL, first_seen INTEGER NOT NULL
 );
 CREATE TABLE IF NOT EXISTS notification_alert_outbox (
 outbox_id TEXT PRIMARY KEY, alert_key TEXT NOT NULL, message TEXT NOT NULL
 );
	`)
	if err != nil {
		return err
	}
	if err = migrateLocalCDKResultColumns(); err != nil {
		return err
	}
	return migrateAutomationDeclineEmails()
}
