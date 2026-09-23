package handler

import (
	"fmt"
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
