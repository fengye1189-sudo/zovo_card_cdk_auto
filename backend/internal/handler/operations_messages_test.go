package handler

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func replyTemplatesFixture(t *testing.T) *gin.Engine {
	t.Helper()
	teamFixture(t)
	if _, err := db.DB.Exec(`CREATE TABLE site_settings(key TEXT PRIMARY KEY,value TEXT NOT NULL,updated_at DATETIME DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("username", "original_owner"); c.Next() })
	r.GET("/templates", AdminReplyTemplates)
	r.PUT("/templates", AdminSaveReplyTemplates)
	return r
}

func callReplyTemplates(r *gin.Engine, method string, body any) (int, map[string]any) {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, "/templates", bytes.NewReader(raw))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var data map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &data)
	return w.Code, data
}

func TestReplyTemplatesVersionConflictPreservesSavedText(t *testing.T) {
	r := replyTemplatesFixture(t)
	status, initial := callReplyTemplates(r, "GET", nil)
	if status != 200 {
		t.Fatal("default read failed", status, initial)
	}
	revision := initial["revision"].(string)
	first := defaultReplyTemplates()
	first["completed"] = "🍁 First saved completion for {order_id}"
	status, saved := callReplyTemplates(r, "PUT", gin.H{"templates": first, "revision": revision})
	if status != 200 {
		t.Fatal("initial save failed", status, saved)
	}
	if saved["revision"] == revision {
		t.Fatal("saved template revision not advanced")
	}
	stale := defaultReplyTemplates()
	stale["completed"] = "stale overwritten message"
	status, _ = callReplyTemplates(r, "PUT", gin.H{"templates": stale, "revision": revision})
	if status != 409 {
		t.Fatal("stale revision not rejected", status)
	}
	status, current := callReplyTemplates(r, "GET", nil)
	if status != 200 || current["templates"].(map[string]any)["completed"] != first["completed"] {
		t.Fatal("stale save overwrote current templates")
	}
	second := defaultReplyTemplates()
	second["completed"] = "🍁 New checked completion for {order_id}"
	status, updated := callReplyTemplates(r, "PUT", gin.H{"templates": second, "revision": current["revision"]})
	if status != 200 || updated["templates"].(map[string]any)["completed"] != second["completed"] {
		t.Fatal("fresh revision could not save", status)
	}
}

func TestConcurrentReplyTemplateSavesDoNotLoseChanges(t *testing.T) {
	r := replyTemplatesFixture(t)
	_, initial := callReplyTemplates(r, "GET", nil)
	revision := initial["revision"]
	results := make(chan int, 2)
	var wg sync.WaitGroup
	for _, suffix := range []string{"A", "B"} {
		wg.Add(1)
		go func(value string) {
			defer wg.Done()
			templates := defaultReplyTemplates()
			templates["completed"] = "Completion " + value
			status, _ := callReplyTemplates(r, "PUT", gin.H{"templates": templates, "revision": revision})
			results <- status
		}(suffix)
	}
	wg.Wait()
	close(results)
	ok, conflict := 0, 0
	for status := range results {
		if status == 200 {
			ok++
		} else if status == 409 {
			conflict++
		} else {
			t.Fatalf("unexpected concurrent status %d", status)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("got %d successful and %d conflict saves", ok, conflict)
	}
}

func TestReplyTemplatesRejectMissingOrUnknownState(t *testing.T) {
	r := replyTemplatesFixture(t)
	_, initial := callReplyTemplates(r, "GET", nil)
	bad := defaultReplyTemplates()
	delete(bad, "review")
	bad["unexpected"] = "Unknown state"
	status, _ := callReplyTemplates(r, "PUT", gin.H{"templates": bad, "revision": initial["revision"]})
	if status != 400 {
		t.Fatal("unknown template state accepted", status)
	}
	if raw, err := db.GetSetting("operations_reply_templates"); err != nil || raw != "" {
		t.Fatal("invalid input changed settings", err)
	}
}
