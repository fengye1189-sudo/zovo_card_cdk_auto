package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReplacementInvalidatesOldPaymentSession(t *testing.T) {
	f := newLocalFixture(t)
	f.router.POST("/replace", LocalCDKReplace)
	cfg := localSettings{Enabled: true, CardID: 123, MinCardBalanceMinor: 2500, Currency: "USD", MaxAmountMinor: 3000, MaxFeeMinor: 100}
	raw, _ := json.Marshal(cfg)
	db.SetSetting("local_cdk_settings", string(raw))
	code := f.code(t)
	status, p := f.call("/preview", gin.H{"code": code})
	if status != 200 {
		t.Fatal(status)
	}
	token := p["redemption_token"]
	cred := gin.H{"mode": "session", "session": strings.Repeat("mock-only-", 8)}
	status, p = f.call("/preflight", gin.H{"redemption_token": token, "credential": cred})
	if status != 200 {
		t.Fatal(status, p)
	}
	body := gin.H{"redemption_token": token, "preflight_token": p["preflight_token"], "credential": cred, "confirmed": true}
	status, p = f.call("/replace", gin.H{"code": code, "request_id": "replace-preflight-0123456", "confirmed": true})
	if status != 200 {
		t.Fatal(status, p)
	}
	status, _ = f.call("/redeem", body)
	if status == 200 || f.calls.Load() != 0 {
		t.Fatal("old session paid after replacement", status)
	}
}

func TestReplacementStockAndLifecycle(t *testing.T) {
	for _, plan := range []string{"plus", "go", "pro_5x", "pro_20x"} {
		t.Run(plan, func(t *testing.T) {
			f := newLocalFixture(t)
			f.router.POST("/replace", LocalCDKReplace)
			if err := MaintainCodeReserve(); err != nil {
				t.Fatal(err)
			}
			var count int
			db.DB.QueryRow("SELECT COUNT(*) FROM local_code_reserve WHERE product_id=?", plan).Scan(&count)
			if count != 5 {
				t.Fatal(count)
			}
			status, data := f.call("/issue", gin.H{"product_id": plan, "days": 1, "count": 1, "request_id": "replacement-batch-012345"})
			if status != 200 {
				t.Fatal(status, data)
			}
			old := data["codes"].([]any)[0].(string)
			expires := int64(data["expires_at"].(float64))
			if expires < time.Now().Add(89*24*time.Hour).Unix() {
				t.Fatal("not fixed ninety days")
			}
			request := gin.H{"code": old, "request_id": "replace-request-0123456789", "confirmed": true}
			status, result := f.call("/replace", request)
			if status != 200 {
				t.Fatal(status, result)
			}
			code := result["code"].(string)
			if code == old || result["expires_at"] != data["expires_at"] || result["product_id"] != plan {
				t.Fatal("incorrect replacement")
			}
			status, _ = f.call("/preview", gin.H{"code": old})
			if status == 200 {
				t.Fatal("old still valid")
			}
			status, result = f.call("/replace", request)
			if status != 200 || result["code"] != code {
				t.Fatal("retry did not recover same code", status, result)
			}
			request["request_id"] = "different-request-012345"
			status, _ = f.call("/replace", request)
			if status != 409 {
				t.Fatal("old exchanged twice")
			}
			status, result = f.call("/preview", gin.H{"code": code})
			if status != 200 || result["plan"] != plan {
				t.Fatal("new code invalid")
			}
			db.DB.QueryRow("SELECT COUNT(*) FROM local_code_reserve WHERE product_id=?", plan).Scan(&count)
			if count != 5 {
				t.Fatal("reserve not refilled")
			}
			var sealed []byte
			db.DB.QueryRow("SELECT encrypted_code FROM local_code_replacements").Scan(&sealed)
			if bytes.Contains(sealed, []byte(code)) {
				t.Fatal("plaintext stored")
			}
		})
	}
}
func TestReplacementRejectsUsedOrUnsafe(t *testing.T) {
	for _, mode := range []string{"reserved", "review", "consumed", "failed", "disabled", "expired", "submitted", "product_disabled"} {
		t.Run(mode, func(t *testing.T) {
			f := newLocalFixture(t)
			f.router.POST("/replace", LocalCDKReplace)
			code := f.code(t)
			switch mode {
			case "expired":
				db.DB.Exec("UPDATE local_cdks SET expires_at=1")
			case "submitted":
				db.DB.Exec("UPDATE local_cdks SET request_id='submitted'")
			case "product_disabled":
				db.DB.Exec("UPDATE operations_products SET enabled=0")
			default:
				db.DB.Exec("UPDATE local_cdks SET status=?", mode)
			}
			status, _ := f.call("/replace", gin.H{"code": code, "request_id": "replace-request-0123456789", "confirmed": true})
			if status != 409 {
				t.Fatal(mode, status)
			}
			var n int
			db.DB.QueryRow("SELECT COUNT(*) FROM local_code_replacements").Scan(&n)
			if n != 0 {
				t.Fatal("unsafe replacement")
			}
		})
	}
}
func TestConcurrentReplacementCreatesOnlyOne(t *testing.T) {
	f := newLocalFixture(t)
	f.router.POST("/replace", LocalCDKReplace)
	code := f.code(t)
	var wg sync.WaitGroup
	statuses := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, _ := f.call("/replace", gin.H{"code": code, "request_id": fmt.Sprintf("unique-request-012345-%d", i), "confirmed": true})
			statuses <- status
		}(i)
	}
	wg.Wait()
	close(statuses)
	success := 0
	for status := range statuses {
		if status == 200 {
			success++
		} else if status != 409 {
			t.Fatal(status)
		}
	}
	if success != 1 {
		t.Fatal("multiple replacements", success)
	}
	var n int
	db.DB.QueryRow("SELECT COUNT(*) FROM local_code_replacements").Scan(&n)
	if n != 1 {
		t.Fatal(n)
	}
}
func TestFullCodeSearchAndDisable(t *testing.T) {
	f := newLocalFixture(t)
	if err := InitOperationsRecords(); err != nil {
		t.Fatal(err)
	}
	f.router.POST("/search", LocalCDKSearch)
	code := f.code(t)
	status, data := f.call("/search", gin.H{"q": strings.ToLower(code)})
	if status != 200 {
		t.Fatal(status, data)
	}
	list := data["list"].([]any)
	if len(list) != 1 {
		t.Fatal("full code lookup failed")
	}
	id := int64(list[0].(map[string]any)["id"].(float64))
	status, _ = f.call(fmt.Sprintf("/disable/%d", id), gin.H{})
	if status != 200 {
		t.Fatal("disable failed", status)
	}
	status, _ = f.call("/preview", gin.H{"code": code})
	if status == 200 {
		t.Fatal("disabled code usable")
	}
}
