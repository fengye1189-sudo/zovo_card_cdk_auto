package handler

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func operationsGet(f *localFixture, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, r)
	return w
}
func TestOperationsRecordExportPaginationAndPrivacy(t *testing.T) {
	f := newLocalFixture(t)
	f.router.GET("/records", OperationsRecordsList)
	f.router.GET("/export", OperationsRecordsExport)
	tx, e := db.DB.Begin()
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 603; i++ {
		batch := "batch-a"
		if i >= 601 {
			batch = "batch-b"
		}
		_, e = tx.Exec("INSERT INTO local_cdks(code_hash,prefix,plan,status,expires_at,created_at,batch_id,token_hash,credential_hash,email,message) VALUES(?,?,?,?,?,?,?,?,?,?,?)", fmt.Sprintf("secret-full-%d", i), fmt.Sprintf("prefix-%d", i), "plus", "unused", 2000000000, 1700000000, batch, "secret-token", "secret-credential", "private@example.com", "=1+1")
		if e != nil {
			t.Fatal(e)
		}
	}
	if tx.Commit() != nil {
		t.Fatal("commit")
	}
	w := operationsGet(f, "/records?batch=batch-a&page=2&page_size=100")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var d struct {
		List  []operationsRecord `json:"list"`
		Total int                `json:"total"`
	}
	if e = json.Unmarshal(w.Body.Bytes(), &d); e != nil {
		t.Fatal(e)
	}
	if d.Total != 601 || len(d.List) != 100 || d.List[0].ID != 501 || d.List[99].ID != 402 {
		t.Fatalf("bad pagination: %+v", d)
	}
	w = operationsGet(f, "/export?batch=batch-a")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	data := w.Body.String()
	for _, secret := range []string{"secret-full", "secret-token", "secret-credential", "private@example.com"} {
		if strings.Contains(data, secret) {
			t.Fatalf("export leaked %s", secret)
		}
	}
	records, e := csv.NewReader(strings.NewReader(strings.TrimPrefix(data, "\ufeff"))).ReadAll()
	if e != nil {
		t.Fatal(e)
	}
	if len(records) != 602 {
		t.Fatalf("export truncated: %d", len(records))
	}
	if records[1][0] != "601" || records[len(records)-1][0] != "1" || records[1][12] != "'=1+1" {
		t.Fatal("unstable order or unsafe CSV", records[1])
	}
	for _, path := range []string{"/records?page=0", "/records?page_size=101", "/records?from=2026-02-31", "/records?from=2026-09-10&to=2026-09-01", "/records?order_id=abc"} {
		if operationsGet(f, path).Code != 400 {
			t.Fatal("accepted bad filter", path)
		}
	}
	if w = operationsGet(f, "/records?q=prefix-1%25"); w.Code != 200 || strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatal("wildcard search was not literal")
	}
}
func TestOperationsCSVInjection(t *testing.T) {
	for _, value := range []string{"=cmd()", " +1", "\t@cmd", "\r-1", "\n=1", "@x", "-1"} {
		if !strings.HasPrefix(operationsCSVCell(value), "'") {
			t.Fatal("unsafe cell", value)
		}
	}
	if operationsCSVCell("MAPLE-123") != "MAPLE-123" {
		t.Fatal("safe cell changed")
	}
}
func TestOperationsBangkokDateBoundary(t *testing.T) {
	f := newLocalFixture(t)
	f.router.GET("/records", OperationsRecordsList)
	f.router.GET("/export", OperationsRecordsExport)
	start, e := time.Parse(time.RFC3339, "2026-09-09T17:00:00Z")
	if e != nil {
		t.Fatal(e)
	}
	for i, created := range []int64{start.Unix() - 1, start.Unix(), start.Add(24*time.Hour).Unix() - 1, start.Add(24 * time.Hour).Unix()} {
		_, e = db.DB.Exec("INSERT INTO local_cdks(code_hash,prefix,plan,expires_at,created_at) VALUES(?,?,?,?,?)", fmt.Sprintf("date-%d", i), "date-prefix", "plus", created+86400, created)
		if e != nil {
			t.Fatal(e)
		}
	}
	w := operationsGet(f, "/records?from=2026-09-10&to=2026-09-10&tz=Asia%2FBangkok")
	var d struct {
		Total int                `json:"total"`
		List  []operationsRecord `json:"list"`
	}
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if e = json.Unmarshal(w.Body.Bytes(), &d); e != nil {
		t.Fatal(e)
	}
	if d.Total != 2 || d.List[0].ID != 3 || d.List[1].ID != 2 {
		t.Fatalf("incorrect Bangkok day: %+v", d)
	}
	w = operationsGet(f, "/export?from=2026-09-10&to=2026-09-10&tz=Asia%2FBangkok")
	if w.Code != 200 {
		t.Fatal("CSV export failed", w.Code, w.Body.String())
	}
	csvRows, e := csv.NewReader(strings.NewReader(strings.TrimPrefix(w.Body.String(), "\ufeff"))).ReadAll()
	if e != nil {
		t.Fatal(e)
	}
	if len(csvRows) != 3 || csvRows[0][6] != "创建时间 Asia/Bangkok" || csvRows[1][6] != "2026-09-10T23:59:59+07:00" || csvRows[2][6] != "2026-09-10T00:00:00+07:00" {
		t.Fatal("CSV created-at column not aligned to Bangkok day", csvRows)
	}
	if operationsGet(f, "/records?tz=invalid").Code != 400 {
		t.Fatal("invalid timezone accepted")
	}
}
func TestOperationsSupportDoesNotAlterPaymentAndPreventsStaleSave(t *testing.T) {
	f := newLocalFixture(t)
	f.router.POST("/support/:id", OperationsSupportSave)
	f.router.GET("/support/:id", OperationsSupportGet)
	f.code(t)
	_, e := db.DB.Exec("UPDATE local_cdks SET status='review',request_id='merchant-one',upstream_id=77 WHERE id=1")
	if e != nil {
		t.Fatal(e)
	}
	s, d := f.call("/support/1", gin.H{"status": "resolved", "assignee": "ops", "conclusion": "已核查，等待人工退款", "note": "仅记录情况", "version": 0})
	if s != 200 {
		t.Fatal(s, d)
	}
	var status, request string
	var upstream int
	if e = db.DB.QueryRow("SELECT status,request_id,upstream_id FROM local_cdks WHERE id=1").Scan(&status, &request, &upstream); e != nil {
		t.Fatal(e)
	}
	if status != "review" || request != "merchant-one" || upstream != 77 || f.calls.Load() != 0 {
		t.Fatal("support changed financial state")
	}
	s, _ = f.call("/support/1", gin.H{"status": "open", "version": 0, "note": "stale"})
	if s != 409 {
		t.Fatal("stale edit accepted", s)
	}
	var count int
	_ = db.DB.QueryRow("SELECT COUNT(*) FROM operations_support_events").Scan(&count)
	if count != 1 {
		t.Fatal("stale event persisted", count)
	}
	s, _ = f.call("/support/1", gin.H{"status": "resolved", "version": 1})
	if s != 400 {
		t.Fatal("empty conclusion accepted")
	}
	w := operationsGet(f, "/support/1")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "仅记录情况") {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestOperationsProductsBindIssuanceAndStopDisabledProduct(t *testing.T) {
	f := newLocalFixture(t)
	f.router.POST("/products", OperationsProductsSave)
	s, d := f.call("/products", gin.H{"id": "team-plus", "name": "团队 Plus", "plan": "plus", "enabled": true, "default_days": 7, "currency": "USD"})
	if s != 200 {
		t.Fatal(s, d)
	}
	s, d = f.call("/issue", gin.H{"count": 1, "request_id": "batch-product-01234567890", "product_id": "team-plus"})
	if s != 200 {
		t.Fatal(s, d)
	}
	if d["plan"] != "plus" || d["product_id"] != "team-plus" {
		t.Fatal(d)
	}
	var productID string
	var expires int64
	if e := db.DB.QueryRow("SELECT b.product_id,l.expires_at FROM operations_product_bindings b JOIN local_cdks l ON l.id=b.local_id").Scan(&productID, &expires); e != nil {
		t.Fatal(e)
	}
	if productID != "team-plus" || expires < time.Now().Add(89*24*time.Hour).Unix() || expires > time.Now().Add(91*24*time.Hour).Unix() {
		t.Fatal(productID, expires)
	}
	_, _ = db.DB.Exec("UPDATE operations_products SET enabled=0 WHERE id='team-plus'")
	s, _ = f.call("/preview", gin.H{"code": d["codes"].([]any)[0]})
	if s != 409 {
		t.Fatal("disabled product preview passed", s)
	}
	s, _ = f.call("/issue", gin.H{"count": 1, "days": 30, "request_id": "another-batch-01234567890", "product_id": "team-plus"})
	if s != 409 {
		t.Fatal("disabled product issued", s)
	}
	s, _ = f.call("/products", gin.H{"id": "unsupported", "name": "Other", "plan": "pro", "enabled": true, "default_days": 30, "currency": "USD"})
	if s != 400 {
		t.Fatal("unsupported plan accepted", s)
	}
}
func TestLocalSettingsRejectStaleDraft(t *testing.T) {
	f := newLocalFixture(t)
	f.router.POST("/settings", LocalCDKPutSettings)
	f.router.GET("/settings", LocalCDKGetSettings)
	w := operationsGet(f, "/settings")
	var d map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	revision := d["revision"]
	body := gin.H{"enabled": false, "card_ids": []int64{}, "max_successful_payments_per_card": 3, "expected_revision": revision}
	s, result := f.call("/settings", body)
	if s != 200 {
		t.Fatal(s, result)
	}
	s, _ = f.call("/settings", body)
	if s != 409 {
		t.Fatal("stale draft overwrote settings", s)
	}
	body["expected_revision"] = result["revision"]
	body["max_successful_payments_per_card"] = 4
	s, result = f.call("/settings", body)
	if s != 200 {
		t.Fatal(s, result)
	}
	if readLocalSettings().MaxSuccessfulPaymentsPerCard != 4 {
		t.Fatal("settings did not persist")
	}
}
