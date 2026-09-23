package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func TestMergeUpstreamCardRemarkPreservesManual(t *testing.T) {
	got, err := mergeUpstreamCardRemark("客户手工备注\n【枫叶自动】旧数据", "【枫叶自动】成功2次：Plus×2；拒付0次")
	if err != nil || got != "客户手工备注 | 【枫叶自动】成功2次：Plus×2；拒付0次" {
		t.Fatalf("bad merged remark %q %v", got, err)
	}
	got, err = mergeUpstreamCardRemark("❌部门团队GPT❌", "【枫叶自动】成功0次；拒付0次")
	if err != nil || got != "部门团队GPT | 【枫叶自动】成功0次；拒付0次" {
		t.Fatalf("legacy unsupported decoration was not safely normalized: %q %v", got, err)
	}
	if _, err = mergeUpstreamCardRemark(strings.Repeat("手", 1200), "【枫叶自动】成功0次；拒付0次"); err == nil {
		t.Fatal("oversized merged remark accepted")
	}
}

func TestSyncUpstreamCardRemarksAggregatesPlans(t *testing.T) {
	newLocalFixture(t)
	_, err := db.DB.Exec(`INSERT INTO local_cdks
	 (id,code_hash,prefix,plan,status,expires_at,created_at,card_id,last_checked,activated_at)
	 VALUES
	 (11,'a','a','plus','consumed',0,100,123,110,120),
	 (12,'b','b','plus','consumed',0,200,123,210,220),
	 (13,'c','c','go','consumed',0,300,123,310,320),
	 (14,'d','d','pro_5x','failed',0,400,123,410,0),
	 (15,'e','e','pro_5x','consumed',0,500,456,510,520);
	 INSERT INTO automation_card_declines(local_id,card_id,status,recorded_at)
	 VALUES(14,123,'declined',410);`)
	if err != nil {
		t.Fatal(err)
	}

	var puts atomic.Int32
	var saved atomic.Value
	current := atomic.Value{}
	current.Store("客户手工备注")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/openapi/v1/cards":
			data = map[string]any{"total": 2, "list": []any{
				map[string]any{"id": 123, "card_number": "5378721111110123", "status": "ACTIVE", "product_code": "TEST", "available_amount": 20, "remark": current.Load().(string)},
				map[string]any{"id": 456, "card_number": "5378721111110456", "status": "ACTIVE", "product_code": "TEST", "available_amount": 100, "remark": ""},
			}}
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/openapi/v1/cards/") && strings.HasSuffix(r.URL.Path, "/remark"):
			var body struct {
				Remark string `json:"remark"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			puts.Add(1)
			saved.Store(body.Remark)
			if r.URL.Path == "/openapi/v1/cards/123/remark" {
				current.Store(body.Remark)
			}
			data = map[string]any{"ok": true}
		default:
			t.Fatalf("unexpected upstream call %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
	}))
	defer srv.Close()
	t.Setenv("CARD_API_BASE", srv.URL)
	t.Setenv("CARD_API_KEY", "test-key")

	syncUpstreamCardRemarks(context.Background())
	if puts.Load() != 2 {
		t.Fatalf("expected both cards to be annotated, got %d", puts.Load())
	}
	remark, _ := saved.Load().(string)
	if !strings.Contains(current.Load().(string), "客户手工备注") ||
		!strings.Contains(current.Load().(string), "成功3次：Plus×2，Go×1") ||
		!strings.Contains(current.Load().(string), "拒付1次") {
		t.Fatal("card 123 summary is incomplete", current.Load())
	}
	if strings.Contains(current.Load().(string), "pro_5x") || strings.Contains(current.Load().(string), "test@example.com") {
		t.Fatal("failed product or private account leaked into the remark")
	}
	if !strings.Contains(remark, "成功1次：Pro 5X×1") {
		t.Fatal("card 456 plan missing", remark)
	}
}
