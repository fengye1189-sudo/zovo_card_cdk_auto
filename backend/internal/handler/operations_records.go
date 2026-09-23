package handler

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func InitOperationsRecords() error {
	_, e := db.DB.Exec(`CREATE TABLE IF NOT EXISTS operations_support (
 local_id INTEGER PRIMARY KEY,status TEXT NOT NULL DEFAULT 'open',assignee TEXT NOT NULL DEFAULT '',
 conclusion TEXT NOT NULL DEFAULT '',version INTEGER NOT NULL DEFAULT 1,updated_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS operations_support_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT,local_id INTEGER NOT NULL,actor TEXT NOT NULL,
 note TEXT NOT NULL,changes TEXT NOT NULL,created_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS idx_operations_support_events_local ON operations_support_events(local_id,id);
 CREATE INDEX IF NOT EXISTS idx_local_cdks_created_id ON local_cdks(created_at,id);
 CREATE INDEX IF NOT EXISTS idx_local_cdks_status_id ON local_cdks(status,id);
 CREATE INDEX IF NOT EXISTS idx_local_cdks_batch ON local_cdks(batch_id);
 CREATE INDEX IF NOT EXISTS idx_local_cdks_card_status ON local_cdks(card_id,status);
 CREATE INDEX IF NOT EXISTS idx_local_cdks_customer_latest ON local_cdks(status,email,activated_at,id);
 CREATE INDEX IF NOT EXISTS idx_local_cdks_subscription_expiry ON local_cdks(status,subscription_expires_at);`)
	return e
}

type operationsRecord struct {
	ID            int64  `json:"id"`
	Prefix        string `json:"prefix"`
	Plan          string `json:"plan"`
	Status        string `json:"status"`
	Expires       int64  `json:"expires_at"`
	Created       int64  `json:"created_at"`
	Batch         string `json:"batch_id"`
	Upstream      int64  `json:"upstream_id"`
	Request       string `json:"request_id"`
	Message       string `json:"message"`
	CardID        int64  `json:"card_id"`
	ProductID     string `json:"product_id"`
	ProductName   string `json:"product_name"`
	SupportStatus string `json:"support_status"`
	Assignee      string `json:"assignee"`
}

const operationsRecordFrom = " FROM local_cdks l LEFT JOIN operations_product_bindings b ON b.local_id=l.id LEFT JOIN operations_products p ON p.id=COALESCE(b.product_id,'plus') LEFT JOIN operations_support s ON s.local_id=l.id"
const operationsRecordColumns = "l.id,l.prefix,l.plan,l.status,l.expires_at,l.created_at,l.batch_id,l.upstream_id,l.request_id,l.message,l.card_id,COALESCE(p.id,'plus'),COALESCE(p.name,'GPT Plus'),COALESCE(s.status,''),COALESCE(s.assignee,'')"

func scanOperationsRecord(row interface{ Scan(...any) error }) (operationsRecord, error) {
	var r operationsRecord
	e := row.Scan(&r.ID, &r.Prefix, &r.Plan, &r.Status, &r.Expires, &r.Created, &r.Batch, &r.Upstream, &r.Request, &r.Message, &r.CardID, &r.ProductID, &r.ProductName, &r.SupportStatus, &r.Assignee)
	return r, e
}
func operationsRecordsLocation(c *gin.Context) (*time.Location, error) {
	switch c.Query("tz") {
	case "", "UTC":
		return time.UTC, nil
	case "Asia/Bangkok":
		return time.FixedZone("Asia/Bangkok", 7*60*60), nil
	default:
		return nil, fmt.Errorf("不支持的日期时区")
	}
}
func operationsRecordsFilter(c *gin.Context) (string, []any, error) {
	location, e := operationsRecordsLocation(c)
	if e != nil {
		return "", nil, e
	}
	clauses := []string{"1=1"}
	args := []any{}
	for _, field := range []struct{ key, column string }{{"status", "l.status"}, {"batch", "l.batch_id"}, {"product", "COALESCE(b.product_id,'plus')"}, {"support_status", "s.status"}, {"assignee", "s.assignee"}} {
		v := strings.TrimSpace(c.Query(field.key))
		if len(v) > 200 {
			return "", nil, fmt.Errorf("筛选条件过长")
		}
		if v != "" {
			clauses = append(clauses, field.column+"=?")
			args = append(args, v)
		}
	}
	if c.Query("orders_only") == "true" {
		clauses = append(clauses, "l.request_id<>''")
	}
	for _, field := range []struct{ key, column string }{{"id", "l.id"}, {"order_id", "l.upstream_id"}, {"card_id", "l.card_id"}} {
		if v := c.Query(field.key); v != "" {
			n, e := strconv.ParseInt(v, 10, 64)
			if e != nil || n < 1 {
				return "", nil, fmt.Errorf("编号需为正整数")
			}
			clauses = append(clauses, field.column+"=?")
			args = append(args, n)
		}
	}
	var from, to time.Time
	for _, field := range []struct{ key, op string }{{"from", ">="}, {"to", "<"}} {
		if v := c.Query(field.key); v != "" {
			t, e := time.ParseInLocation("2006-01-02", v, location)
			if e != nil {
				return "", nil, fmt.Errorf("日期格式应为 YYYY-MM-DD")
			}
			if field.key == "to" {
				to = t
				t = t.AddDate(0, 0, 1)
			} else {
				from = t
			}
			clauses = append(clauses, "l.created_at"+field.op+"?")
			args = append(args, t.Unix())
		}
	}
	if !from.IsZero() && !to.IsZero() && to.Before(from) {
		return "", nil, fmt.Errorf("结束日期不能早于开始日期")
	}
	if q := strings.TrimSpace(c.Query("q")); q != "" {
		if len(q) > 200 {
			return "", nil, fmt.Errorf("搜索内容过长")
		}
		q = strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(q)
		v := "%" + q + "%"
		clauses = append(clauses, `(l.code_hash=? OR l.prefix LIKE ? ESCAPE '\' OR l.batch_id LIKE ? ESCAPE '\' OR l.request_id LIKE ? ESCAPE '\' OR CAST(l.id AS TEXT)=? OR CAST(l.upstream_id AS TEXT)=?)`)
		args = append(args, localHash(strings.ToUpper(strings.TrimSpace(c.Query("q")))), v, v, v, c.Query("q"), c.Query("q"))
	}
	return " WHERE " + strings.Join(clauses, " AND "), args, nil
}
func operationsPage(c *gin.Context) (int, int, error) {
	page, size := 1, 25
	var e error
	if v := c.Query("page"); v != "" {
		page, e = strconv.Atoi(v)
		if e != nil {
			return 0, 0, e
		}
	}
	if v := c.Query("page_size"); v != "" {
		size, e = strconv.Atoi(v)
		if e != nil {
			return 0, 0, e
		}
	}
	if page < 1 || page > 100000000 || size < 1 || size > 100 {
		return 0, 0, fmt.Errorf("invalid pagination")
	}
	return page, size, nil
}
func OperationsRecordsList(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	where, args, e := operationsRecordsFilter(c)
	if e != nil {
		localError(c, 400, e.Error())
		return
	}
	page, size, e := operationsPage(c)
	if e != nil {
		localError(c, 400, "页码或每页数量不正确")
		return
	}
	tx, e := db.DB.BeginTx(c.Request.Context(), &sql.TxOptions{ReadOnly: true})
	if e != nil {
		localError(c, 503, "读取繁忙，请重试")
		return
	}
	defer tx.Rollback()
	var total int64
	if e = tx.QueryRow("SELECT COUNT(*)"+operationsRecordFrom+where, args...).Scan(&total); e != nil {
		localError(c, 500, "读取记录失败")
		return
	}
	queryArgs := append(append([]any{}, args...), size, int64(page-1)*int64(size))
	rows, e := tx.Query("SELECT "+operationsRecordColumns+operationsRecordFrom+where+" ORDER BY l.id DESC LIMIT ? OFFSET ?", queryArgs...)
	if e != nil {
		localError(c, 500, "读取记录失败")
		return
	}
	defer rows.Close()
	list := []operationsRecord{}
	for rows.Next() {
		r, e := scanOperationsRecord(rows)
		if e != nil {
			localError(c, 500, "读取记录失败")
			return
		}
		list = append(list, r)
	}
	if rows.Err() != nil {
		localError(c, 500, "读取记录失败")
		return
	}
	location, _ := operationsRecordsLocation(c)
	c.JSON(200, gin.H{"list": list, "total": total, "page": page, "page_size": size, "date_timezone": location.String()})
}
func operationsCSVCell(value string) string {
	trimmed := strings.TrimLeftFunc(value, unicode.IsSpace)
	if trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + value
	}
	if strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") || strings.HasPrefix(value, "\n") {
		return "'" + value
	}
	return value
}
func OperationsRecordsExport(c *gin.Context) {
	where, args, e := operationsRecordsFilter(c)
	if e != nil {
		localError(c, 400, e.Error())
		return
	}
	tx, e := db.DB.BeginTx(c.Request.Context(), &sql.TxOptions{ReadOnly: true})
	if e != nil {
		localError(c, 503, "导出繁忙，请重试")
		return
	}
	defer tx.Rollback()
	// One read snapshot, no LIMIT: exported rows exactly match this filter at the
	// moment the export starts, including records beyond the first 500.
	rows, e := tx.QueryContext(c.Request.Context(), "SELECT "+operationsRecordColumns+operationsRecordFrom+where+" ORDER BY l.id DESC", args...)
	if e != nil {
		localError(c, 500, "导出读取失败")
		return
	}
	defer rows.Close()
	output, e := os.CreateTemp("", "maple-record-export-*.csv")
	if e != nil {
		localError(c, 500, "无法创建导出文件")
		return
	}
	defer os.Remove(output.Name())
	defer output.Close()
	_, e = output.Write([]byte{0xEF, 0xBB, 0xBF})
	if e != nil {
		localError(c, 500, "导出写入失败")
		return
	}
	w := csv.NewWriter(output)
	location, _ := operationsRecordsLocation(c)
	_ = w.Write([]string{"ID", "卡密前缀", "商品编号", "商品名称", "套餐", "支付状态", "创建时间 " + location.String(), "到期时间 " + location.String(), "批次", "商户订单号", "上游订单ID", "支付卡ID", "说明", "售后状态", "处理人"})
	for rows.Next() {
		r, e := scanOperationsRecord(rows)
		if e != nil {
			localError(c, 500, "导出读取失败，文件未下载")
			return
		}
		values := []string{strconv.FormatInt(r.ID, 10), r.Prefix, r.ProductID, r.ProductName, r.Plan, r.Status, time.Unix(r.Created, 0).In(location).Format(time.RFC3339), time.Unix(r.Expires, 0).In(location).Format(time.RFC3339), r.Batch, r.Request, strconv.FormatInt(r.Upstream, 10), strconv.FormatInt(r.CardID, 10), r.Message, r.SupportStatus, r.Assignee}
		for i := range values {
			values[i] = operationsCSVCell(values[i])
		}
		if e = w.Write(values); e != nil {
			localError(c, 500, "导出写入失败，文件未下载")
			return
		}
	}
	if rows.Err() != nil {
		localError(c, 500, "导出读取失败，文件未下载")
		return
	}
	w.Flush()
	if w.Error() != nil {
		localError(c, 500, "导出写入失败，文件未下载")
		return
	}
	rows.Close()
	_ = tx.Rollback()
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.FileAttachment(output.Name(), "maple-records.csv")
	db.WriteAudit(c.GetString("username"), "operations_records_export", "filtered CSV", c.ClientIP())
}

type operationsSupport struct {
	Status     string `json:"status"`
	Assignee   string `json:"assignee"`
	Conclusion string `json:"conclusion"`
	Version    int64  `json:"version"`
	UpdatedAt  int64  `json:"updated_at"`
}

func OperationsSupportGet(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, e := strconv.ParseInt(c.Param("id"), 10, 64)
	if e != nil || id < 1 {
		localError(c, 400, "记录编号不正确")
		return
	}
	r, e := scanOperationsRecord(db.DB.QueryRow("SELECT "+operationsRecordColumns+operationsRecordFrom+" WHERE l.id=?", id))
	if e == sql.ErrNoRows {
		localError(c, 404, "记录不存在")
		return
	}
	if e != nil {
		localError(c, 500, "读取失败")
		return
	}
	support := operationsSupport{Status: "open"}
	e = db.DB.QueryRow("SELECT status,assignee,conclusion,version,updated_at FROM operations_support WHERE local_id=?", id).Scan(&support.Status, &support.Assignee, &support.Conclusion, &support.Version, &support.UpdatedAt)
	if e != nil && e != sql.ErrNoRows {
		localError(c, 500, "读取售后失败")
		return
	}
	events := []gin.H{}
	rows, e := db.DB.Query("SELECT id,actor,note,changes,created_at FROM operations_support_events WHERE local_id=? ORDER BY id DESC", id)
	if e != nil {
		localError(c, 500, "读取历史失败")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var eventID, created int64
		var actor, note, changes string
		if rows.Scan(&eventID, &actor, &note, &changes, &created) != nil {
			localError(c, 500, "读取历史失败")
			return
		}
		var change any
		_ = json.Unmarshal([]byte(changes), &change)
		events = append(events, gin.H{"id": eventID, "actor": actor, "note": note, "changes": change, "created_at": created})
	}
	if rows.Err() != nil {
		localError(c, 500, "读取历史失败")
		return
	}
	c.JSON(200, gin.H{"record": r, "support": support, "events": events})
}
func OperationsSupportSave(c *gin.Context) {
	id, e := strconv.ParseInt(c.Param("id"), 10, 64)
	if e != nil || id < 1 {
		localError(c, 400, "记录编号不正确")
		return
	}
	var req struct {
		operationsSupport
		Note string `json:"note"`
	}
	if !localBody(c, &req) {
		return
	}
	req.Assignee = strings.TrimSpace(req.Assignee)
	req.Conclusion = strings.TrimSpace(req.Conclusion)
	req.Note = strings.TrimSpace(req.Note)
	if (req.Status != "open" && req.Status != "in_progress" && req.Status != "resolved") || len([]rune(req.Assignee)) > 80 || len([]rune(req.Conclusion)) > 2000 || len([]rune(req.Note)) > 4000 || req.Version < 0 || (req.Status == "resolved" && req.Conclusion == "") {
		localError(c, 400, "请检查售后状态、处理人和备注；结案必须填写处理结论")
		return
	}
	tx, e := db.DB.Begin()
	if e != nil {
		localError(c, 503, "保存繁忙")
		return
	}
	defer tx.Rollback()
	var exists int
	if e = tx.QueryRow("SELECT COUNT(*) FROM local_cdks WHERE id=?", id).Scan(&exists); e != nil {
		localError(c, 500, "查询记录失败")
		return
	}
	if exists != 1 {
		localError(c, 404, "记录不存在")
		return
	}
	old := operationsSupport{Status: "open"}
	e = tx.QueryRow("SELECT status,assignee,conclusion,version,updated_at FROM operations_support WHERE local_id=?", id).Scan(&old.Status, &old.Assignee, &old.Conclusion, &old.Version, &old.UpdatedAt)
	if e != nil && e != sql.ErrNoRows {
		localError(c, 500, "读取售后失败")
		return
	}
	if old.Version != req.Version {
		localError(c, 409, "售后已被其他管理员更新，请重新打开后再保存")
		return
	}
	if old.Status == req.Status && old.Assignee == req.Assignee && old.Conclusion == req.Conclusion && req.Note == "" {
		localError(c, 400, "请填写备注或修改售后信息")
		return
	}
	now := time.Now().Unix()
	_, e = tx.Exec("INSERT INTO operations_support(local_id,status,assignee,conclusion,version,updated_at) VALUES(?,?,?,?,1,?) ON CONFLICT(local_id) DO UPDATE SET status=excluded.status,assignee=excluded.assignee,conclusion=excluded.conclusion,version=operations_support.version+1,updated_at=excluded.updated_at", id, req.Status, req.Assignee, req.Conclusion, now)
	if e != nil {
		localError(c, 503, "保存售后失败，请刷新重试")
		return
	}
	changes, _ := json.Marshal(gin.H{"before": old, "after": gin.H{"status": req.Status, "assignee": req.Assignee, "conclusion": req.Conclusion}})
	_, e = tx.Exec("INSERT INTO operations_support_events(local_id,actor,note,changes,created_at) VALUES(?,?,?,?,?)", id, c.GetString("username"), req.Note, string(changes), now)
	if e != nil {
		localError(c, 500, "保存历史失败")
		return
	}
	if e = tx.Commit(); e != nil {
		localError(c, 503, "保存结果待确认，请重新打开核对")
		return
	}
	db.WriteAudit(c.GetString("username"), "operations_support_save", fmt.Sprintf("local_id=%d status=%s", id, req.Status), c.ClientIP())
	c.JSON(200, gin.H{"ok": true, "version": old.Version + 1})
}
