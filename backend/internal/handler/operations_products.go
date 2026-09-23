package handler

import (
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

// Products bind immutable fulfillment plans. Limits are applied by plan at
// preflight and submission, independently of display-only reference prices.
type OperationsProduct struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Description         string `json:"description"`
	Plan                string `json:"plan"`
	Enabled             bool   `json:"enabled"`
	DefaultDays         int    `json:"default_days"`
	ReferencePriceMinor int64  `json:"reference_price_minor"`
	Currency            string `json:"currency"`
	Version             int64  `json:"version"`
	UpdatedAt           int64  `json:"updated_at"`
}

func InitOperationsProducts() error {
	_, e := db.DB.Exec(`CREATE TABLE IF NOT EXISTS operations_products (
 id TEXT PRIMARY KEY,name TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',
 plan TEXT NOT NULL CHECK(plan='plus'),enabled INTEGER NOT NULL DEFAULT 1,
 default_days INTEGER NOT NULL DEFAULT 30,reference_price_minor INTEGER NOT NULL DEFAULT 0,
 currency TEXT NOT NULL DEFAULT 'USD',version INTEGER NOT NULL DEFAULT 1,updated_at INTEGER NOT NULL);
 INSERT OR IGNORE INTO operations_products(id,name,plan,updated_at) VALUES('plus','GPT Plus','plus',strftime('%s','now'));
 CREATE TABLE IF NOT EXISTS operations_product_bindings(local_id INTEGER PRIMARY KEY,product_id TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS idx_operations_product_bindings_product ON operations_product_bindings(product_id);`)
	if e != nil {
		return e
	}
	if e = migrateLocalPlanProducts(); e != nil {
		return e
	}
	_, e = db.DB.Exec("UPDATE operations_products SET default_days=90 WHERE default_days<>90")
	return e
}

const operationsProductColumns = "id,name,description,plan,enabled,default_days,reference_price_minor,currency,version,updated_at"

func scanOperationsProduct(row interface{ Scan(...any) error }) (OperationsProduct, error) {
	var p OperationsProduct
	e := row.Scan(&p.ID, &p.Name, &p.Description, &p.Plan, &p.Enabled, &p.DefaultDays, &p.ReferencePriceMinor, &p.Currency, &p.Version, &p.UpdatedAt)
	return p, e
}
func operationsProductForIssue(id string) (OperationsProduct, error) {
	if id == "" {
		id = "plus"
	}
	p, e := scanOperationsProduct(db.DB.QueryRow("SELECT "+operationsProductColumns+" FROM operations_products WHERE id=?", id))
	if e != nil {
		return p, e
	}
	if !p.Enabled || !supportedLocalPlan(p.Plan) {
		return p, errors.New("product unavailable")
	}
	return p, nil
}
func operationsProductAvailable(localID int64) bool {
	var enabled bool
	var plan string
	e := db.DB.QueryRow("SELECT p.enabled,p.plan FROM operations_products p WHERE p.id=COALESCE((SELECT product_id FROM operations_product_bindings WHERE local_id=?),'plus')", localID).Scan(&enabled, &plan)
	return e == nil && enabled && supportedLocalPlan(plan)
}

func OperationsProductsList(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	rows, e := db.DB.Query("SELECT " + operationsProductColumns + " FROM operations_products ORDER BY id")
	if e != nil {
		localError(c, 500, "商品读取失败")
		return
	}
	defer rows.Close()
	list := []OperationsProduct{}
	for rows.Next() {
		p, e := scanOperationsProduct(rows)
		if e != nil {
			localError(c, 500, "商品读取失败")
			return
		}
		list = append(list, p)
	}
	if rows.Err() != nil {
		localError(c, 500, "商品读取失败")
		return
	}
	c.JSON(200, gin.H{"list": list, "supported_plans": []string{"plus", "go", "pro_5x", "pro_20x"}, "price_note": "参考售价仅用于商品记录，不会修改充值报价或资金限额"})
}

var operationsProductID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)

func OperationsProductsSave(c *gin.Context) {
	var p OperationsProduct
	if !localBody(c, &p) {
		return
	}
	p.ID = strings.TrimSpace(p.ID)
	p.DefaultDays = 90
	p.Name = strings.TrimSpace(p.Name)
	p.Description = strings.TrimSpace(p.Description)
	p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
	if c.Request.Method == "PUT" {
		p.ID = c.Param("id")
	}
	if !operationsProductID.MatchString(p.ID) || p.Name == "" || len([]rune(p.Name)) > 80 || len([]rune(p.Description)) > 1000 || !supportedLocalPlan(p.Plan) || p.DefaultDays < 1 || p.DefaultDays > 365 || p.ReferencePriceMinor < 0 || p.ReferencePriceMinor > 100000000 || !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(p.Currency) {
		localError(c, 400, "请检查商品编号、名称、有效期、币种和参考售价；当前支持 Plus、Go、Pro 5X、Pro 20X")
		return
	}
	p.UpdatedAt = time.Now().Unix()
	if c.Request.Method == "POST" {
		var exists int
		if e := db.DB.QueryRow("SELECT COUNT(*) FROM operations_products WHERE id=?", p.ID).Scan(&exists); e != nil {
			localError(c, 500, "商品读取失败")
			return
		}
		if exists > 0 {
			localError(c, 409, "商品编号已存在")
			return
		}
		_, e := db.DB.Exec("INSERT INTO operations_products(id,name,description,plan,enabled,default_days,reference_price_minor,currency,updated_at) VALUES(?,?,?,?,?,?,?,?,?)", p.ID, p.Name, p.Description, p.Plan, p.Enabled, p.DefaultDays, p.ReferencePriceMinor, p.Currency, p.UpdatedAt)
		if e != nil {
			localError(c, 409, "新增失败，请刷新核对商品编号")
			return
		}
		p.Version = 1
	} else {
		if p.Version < 1 {
			localError(c, 400, "缺少商品版本，请刷新后重试")
			return
		}
		result, e := db.DB.Exec("UPDATE operations_products SET name=?,description=?,enabled=?,default_days=?,reference_price_minor=?,currency=?,version=version+1,updated_at=? WHERE id=? AND version=? AND plan=?", p.Name, p.Description, p.Enabled, p.DefaultDays, p.ReferencePriceMinor, p.Currency, p.UpdatedAt, p.ID, p.Version, p.Plan)
		if e != nil {
			localError(c, 500, "保存失败")
			return
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			localError(c, 409, "商品已被其他管理员修改或不存在，请刷新后重试")
			return
		}
		p.Version++
	}
	db.WriteAudit(c.GetString("username"), "operations_product_save", p.ID, c.ClientIP())
	c.JSON(200, gin.H{"product": p})
}

// Verify at issuance inside the same transaction that creates the cards, so a
// concurrent product disable cannot leave a partially issued batch.
func operationsBindIssuedProduct(tx *sql.Tx, localID int64, productID string) error {
	_, e := tx.Exec("INSERT INTO operations_product_bindings(local_id,product_id) VALUES(?,?)", localID, productID)
	return e
}
