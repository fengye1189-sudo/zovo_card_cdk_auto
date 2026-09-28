package handler

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func insertCustomerCompletion(t *testing.T, key, email, plan string, activated, expires int64, estimated int) {
	t.Helper()
	_, err := db.DB.Exec(`INSERT INTO local_cdks
		(code_hash,prefix,plan,status,expires_at,created_at,email,activated_at,
		 subscription_expires_at,upgrade_type,expiry_estimated,upstream_id,card_id)
		VALUES(?,?,?,'consumed',?,?,?,?,?,?,?,?,?)`,
		key, key, plan, expires+86400, activated-60, email, activated, expires,
		localUpgradeType(plan), estimated, activated, activated+1000)
	if err != nil {
		t.Fatal(err)
	}
}

func TestOperationsCustomerDashboardDeduplicatesLatestAccountAndFiltersExpiry(t *testing.T) {
	f := newLocalFixture(t)
	f.router.POST("/customers/search", OperationsCustomersSearch)
	now := time.Now().Unix()

	insertCustomerCompletion(t, "old-a", "CustomerA@Example.com", "plus", now-10*86400, now+20*86400, 1)
	insertCustomerCompletion(t, "new-a", "customera@example.com", "pro_5x", now-86400, now+29*86400, 1)
	insertCustomerCompletion(t, "today-b", "b@example.com", "plus", now, now+30*86400, 1)
	insertCustomerCompletion(t, "soon-c", "c@example.com", "plus", now-5*86400, now+2*86400, 1)
	insertCustomerCompletion(t, "expired-d", "d@example.com", "plus", now-40*86400, now-86400, 0)
	if _, err := db.DB.Exec(`INSERT INTO local_cdks
		(code_hash,prefix,plan,status,expires_at,created_at,email)
		VALUES('missing','missing','plus','consumed',?,?, '')`, now+86400, now); err != nil {
		t.Fatal(err)
	}

	status, body := f.call("/customers/search", gin.H{
		"page": 1, "page_size": 25, "state": "active", "timezone": "Asia/Bangkok",
	})
	if status != 200 {
		t.Fatal(status, body)
	}
	if body["total"] != float64(3) {
		t.Fatalf("latest-account active total was not deduplicated: %+v", body)
	}
	plans := map[string]map[string]any{}
	for _, raw := range body["plans"].([]any) {
		item := raw.(map[string]any)
		plans[item["plan"].(string)] = item
	}
	if plans["plus"]["valid_accounts"] != float64(2) || plans["plus"]["new_today"] != float64(1) || plans["plus"]["expiring_3d"] != float64(1) {
		t.Fatal("incorrect Plus retention summary", plans["plus"])
	}
	if plans["pro_5x"]["valid_accounts"] != float64(1) || plans["pro_20x"]["valid_accounts"] != float64(0) {
		t.Fatal("incorrect Pro retention summary", plans)
	}
	quality := body["data_quality"].(map[string]any)
	if quality["completed"] != float64(6) || quality["missing_email"] != float64(1) || quality["missing_dates"] != float64(1) {
		t.Fatal("data quality totals are incomplete", quality)
	}

	status, body = f.call("/customers/search", gin.H{
		"page": 1, "page_size": 25, "expiry_days": 3, "timezone": "UTC",
	})
	if status != 200 || body["total"] != float64(1) {
		t.Fatal("3-day expiry filter failed", status, body)
	}
	row := body["list"].([]any)[0].(map[string]any)
	if row["email"] != "c@example.com" || row["expiry_estimated"] != true {
		t.Fatal("wrong expiring customer", row)
	}
	if row["buyer_name"] != "升级账号本人" || row["buyer_email"] != "c@example.com" || row["identity_source"] != "upgrade_email_inferred" {
		t.Fatal("unlinked customer did not receive an explicit email-based inference", row)
	}

	status, body = f.call("/customers/search", gin.H{
		"page": 1, "page_size": 25, "plan": "plus", "segment": "new_today", "timezone": "Asia/Bangkok",
	})
	if status != 200 || body["total"] != float64(1) {
		t.Fatal("today metric segment failed", status, body)
	}
	row = body["list"].([]any)[0].(map[string]any)
	if row["email"] != "b@example.com" {
		t.Fatal("today metric opened the wrong customer", row)
	}

	status, body = f.call("/customers/search", gin.H{
		"page": 1, "page_size": 25, "state": "all", "q": "CUSTOMERA@", "timezone": "Asia/Bangkok",
	})
	if status != 200 || body["total"] != float64(1) {
		t.Fatal("literal case-insensitive account search failed", status, body)
	}
	row = body["list"].([]any)[0].(map[string]any)
	if row["plan"] != "pro_5x" || row["email"] != "customera@example.com" {
		t.Fatal("older plan was counted as current", row)
	}
}

func TestOperationsCustomerDashboardRejectsUnsafeFilters(t *testing.T) {
	f := newLocalFixture(t)
	f.router.POST("/customers/search", OperationsCustomersSearch)
	bad := []gin.H{
		{"page": 0, "page_size": 101},
		{"state": "unknown"},
		{"plan": "enterprise"},
		{"expiry_days": 5},
		{"segment": "unknown"},
		{"timezone": "Mars/Base"},
		{"q": strings.Repeat("x", 255)},
	}
	for i, body := range bad {
		status, response := f.call("/customers/search", body)
		if status != 400 {
			t.Fatalf("bad filter %d accepted: %d %v", i, status, response)
		}
	}
	now := time.Now().Unix()
	if _, _, err := operationsCustomerFilters(operationsCustomerSearch{Query: "literal%_\\"}, now, now-3600, now+23*3600); err != nil {
		t.Fatal(fmt.Errorf("literal search rejected: %w", err))
	}
}

func TestOperationsCustomersIncludesManagedJZCompletionAndLatestRenewal(t *testing.T) {
	f := newLocalFixture(t)
	f.router.POST("/customers/search", OperationsCustomersSearch)
	now := time.Now().UTC()
	codeHash := strings.Repeat("e", 64)
	if err := db.BeginManagedActivation(codeHash, strings.Repeat("f", 64), "plus", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	activated := now.Add(-30 * time.Minute)
	expires := activated.AddDate(0, 1, 0)
	if err := db.RecordManagedActivationResultDetails(codeHash, "JZ-TASK-1", "plus", "completed",
		"jz-member@example.com", "", activated.Unix(), expires.Unix(), now); err != nil {
		t.Fatal(err)
	}
	orderID := "018f27ef-7a39-7e91-89ab-cdef01234567"
	if _, err := db.DB.Exec(`INSERT INTO marketplace_managed_cdk_bindings(code_hash,marketplace_order_id,created_at) VALUES(?,?,?)`, codeHash, orderID, now.Unix()); err != nil {
		t.Fatal(err)
	}
	status, body := f.call("/customers/search", gin.H{
		"page": 1, "page_size": 25, "state": "active", "timezone": "UTC",
	})
	if status != http.StatusOK || body["total"] != float64(1) {
		t.Fatalf("managed customer missing: status=%d body=%v", status, body)
	}
	row := body["list"].([]any)[0].(map[string]any)
	if row["email"] != "jz-member@example.com" || row["plan"] != "plus" || row["marketplace_order_id"] != orderID || row["expiry_estimated"] != true {
		t.Fatalf("managed customer fields=%v", row)
	}
	plans := body["plans"].([]any)
	if plans[0].(map[string]any)["valid_accounts"] != float64(1) {
		t.Fatalf("managed completion missing from Plus summary: %v", plans)
	}
	if err := InitCustomerExpiryNotifications(); err != nil {
		t.Fatal(err)
	}
	if err := seedCustomerExpiryNotifications(now.Unix()); err != nil {
		t.Fatal(err)
	}
	var reminderOrder, reminderEmail string
	var dueAt int64
	if err := db.DB.QueryRow(`SELECT marketplace_order_id,account_email,due_at
		FROM customer_expiry_notifications`).Scan(&reminderOrder, &reminderEmail, &dueAt); err != nil {
		t.Fatal(err)
	}
	if reminderOrder != orderID || reminderEmail != "jz-member@example.com" || dueAt != expires.Unix() {
		t.Fatalf("managed renewal reminder lost buyer binding: order=%q email=%q due=%d", reminderOrder, reminderEmail, dueAt)
	}
	if early, err := dueCustomerExpiryNotifications(expires.Add(-24*time.Hour).Unix(), 10); err != nil || len(early) != 0 {
		t.Fatalf("renewal reminder was sent before the expiry day: items=%v err=%v", early, err)
	}
	if due, err := dueCustomerExpiryNotifications(expires.Unix(), 10); err != nil || len(due) != 1 {
		t.Fatalf("renewal reminder was not due at expiry: items=%v err=%v", due, err)
	}
}
