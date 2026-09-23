package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
	"strings"
	"testing"
	"time"
)

func TestIssuedCodeLabelsAndStoredPrefixes(t *testing.T) {
	for _, plan := range []string{"plus", "go", "pro_5x", "pro_20x"} {
		t.Run(plan, func(t *testing.T) {
			f := newLocalFixture(t)
			status, d := f.call("/issue", gin.H{"product_id": plan, "count": 1, "days": 30, "request_id": "label-batch-012345678901"})
			if status != 200 {
				t.Fatal(status, d)
			}
			code := d["codes"].([]any)[0].(string)
			head := map[string]string{"plus": "PULS-", "go": "GO-", "pro_5x": "PRO5X-", "pro_20x": "PRO-"}[plan]
			suffix := strings.TrimPrefix(code, head)
			if !strings.HasPrefix(code, head) || len(suffix) != 15 || strings.Trim(suffix, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
				t.Fatal("expected product label and fifteen uppercase letters")
			}
			var prefix string
			if e := db.DB.QueryRow("SELECT prefix FROM local_cdks WHERE code_hash=?", localHash(code)).Scan(&prefix); e != nil || prefix != code[:len(head)+4] {
				t.Fatal("incorrect masked prefix", e)
			}
			status, d = f.call("/preview", gin.H{"code": strings.ToLower(code)})
			if status != 200 || d["plan"] != plan {
				t.Fatal("new code preview failed", status, d)
			}
			other := "OTHER-" + suffix
			status, _ = f.call("/preview", gin.H{"code": other})
			if status == 200 {
				t.Fatal("modified label accepted")
			}
		})
	}
}

func TestShortCodeBatchUnique(t *testing.T) {
	f := newLocalFixture(t)
	status, d := f.call("/issue", gin.H{"product_id": "plus", "count": 100, "days": 30, "request_id": "short-batch-012345678901"})
	if status != 200 {
		t.Fatal(status, d)
	}
	seen := map[string]bool{}
	for _, v := range d["codes"].([]any) {
		code := v.(string)
		suffix := strings.TrimPrefix(code, "PULS-")
		if len(code) != 20 || len(suffix) != 15 || strings.Trim(suffix, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" || seen[code] {
			t.Fatal("invalid or duplicate code")
		}
		seen[code] = true
	}
}

func TestLegacyUnlabelledCodeStillValid(t *testing.T) {
	for _, code := range []string{"MAPLE-" + strings.Repeat("A", 48), strings.Repeat("B", 15), "MAPLE-PLUS-" + strings.Repeat("C", 48)} {
		f := newLocalFixture(t)
		_, e := db.DB.Exec("INSERT INTO local_cdks(code_hash,prefix,plan,expires_at,created_at) VALUES(?,?,?,?,?)", localHash(code), code[:14], "plus", time.Now().Add(time.Hour).Unix(), time.Now().Unix())
		if e != nil {
			t.Fatal(e)
		}
		status, d := f.call("/preview", gin.H{"code": code})
		if status != 200 || d["plan"] != "plus" {
			t.Fatal("legacy code rejected", status, d)
		}
	}
}
