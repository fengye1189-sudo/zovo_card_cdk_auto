package handler

import (
	"github.com/tuzi/cdk-recharge-system/internal/db"
	"strings"
)

func migrateLocalPlanProducts() error {
	tx, e := db.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var schema string
	if e = tx.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='operations_products'").Scan(&schema); e != nil {
		return e
	}
	if !strings.Contains(schema, "'credit25000'") {
		_, e = tx.Exec(`CREATE TABLE operations_products_multiplan (
 id TEXT PRIMARY KEY,name TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',
 plan TEXT NOT NULL CHECK(plan IN ('plus','go','pro_5x','pro_5x_cl','pro_20x','credit250','credit500','credit1000','credit2500','credit5000','credit25000')),enabled INTEGER NOT NULL DEFAULT 1,
 default_days INTEGER NOT NULL DEFAULT 30,reference_price_minor INTEGER NOT NULL DEFAULT 0,
 currency TEXT NOT NULL DEFAULT 'USD',version INTEGER NOT NULL DEFAULT 1,updated_at INTEGER NOT NULL);
 INSERT INTO operations_products_multiplan SELECT * FROM operations_products;
 DROP TABLE operations_products;
 ALTER TABLE operations_products_multiplan RENAME TO operations_products;`)
		if e != nil {
			return e
		}
	}
	_, e = tx.Exec(`INSERT OR IGNORE INTO operations_products(id,name,plan,currency,updated_at) VALUES
 ('go','ChatGPT Go','go','PHP',strftime('%s','now')),
 ('pro_5x','GPTPRO5x卡冲升级','pro_5x','PHP',strftime('%s','now')),
 ('pro_5x_cl','ChatGPT Pro 5X 智利区充值','pro_5x_cl','CLP',strftime('%s','now')),
 ('pro_20x','ChatGPT Pro 20X','pro_20x','PHP',strftime('%s','now')),
 ('credit250','Codex 点数 250','credit250','PHP',strftime('%s','now')),
 ('credit500','Codex 点数 500','credit500','PHP',strftime('%s','now')),
 ('credit1000','Codex 点数 1000','credit1000','PHP',strftime('%s','now')),
 ('credit2500','Codex 点数 2500','credit2500','PHP',strftime('%s','now')),
 ('credit5000','Codex 点数 5000','credit5000','PHP',strftime('%s','now')),
 ('credit25000','Codex 点数 25000','credit25000','PHP',strftime('%s','now'));`)
	if e != nil {
		return e
	}
	return tx.Commit()
}
