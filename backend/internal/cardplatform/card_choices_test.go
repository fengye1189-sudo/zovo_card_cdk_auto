package cardplatform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestCardChoicesMaskSensitiveFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/openapi/v1/cards" || r.URL.Query().Get("page") != "2" || r.URL.Query().Get("page_size") != "20" || r.URL.Query().Get("sync") != "" {
			t.Error("unexpected request")
		}
		w.Write([]byte(`{"code":0,"data":{"total":21,"list":[{"id":1,"card_number":"1111 2222 3333 4444","cvv":"secret-cvv","first_name":"private-name","expire":"secret-expiry","remark":"private-card-note","available_amount":10.12,"status":"ACTIVE","product_code":"test"}]}}`))
	}))
	defer srv.Close()
	cards, total, e := New(Config{SiteBase: srv.URL, APIKey: "test-only"}).CardChoices(context.Background(), 2)
	if e != nil || total != 21 || len(cards) != 1 || cards[0].Last4 != "4444" || cards[0].Balance == nil || *cards[0].Balance != 10.12 {
		t.Fatalf("bad list %v", e)
	}
	raw, _ := json.Marshal(cards)
	if cards[0].Remark != "private-card-note" {
		t.Fatal("server-side card remark missing")
	}
	for _, secret := range []string{"1111", "2222", "3333", "secret-cvv", "private-name", "secret-expiry", "private-card-note", "card_number"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("sensitive field exposed")
		}
	}
}

func TestSetCardRemark(t *testing.T) {
	var saved string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/openapi/v1/cards/123/remark" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Remark string `json:"remark"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		saved = body.Remark
		w.Write([]byte(`{"code":0,"data":{"ok":true}}`))
	}))
	defer srv.Close()
	client := New(Config{SiteBase: srv.URL, APIKey: "test-only"})
	if err := client.SetCardRemark(context.Background(), 123, "  usage summary  "); err != nil || saved != "usage summary" {
		t.Fatalf("remark not saved: %q %v", saved, err)
	}
	if err := client.SetCardRemark(context.Background(), 123, strings.Repeat("x", MaxCardRemarkRunes+1)); err == nil {
		t.Fatal("oversized remark accepted")
	}
}

func TestUnarchivedCardChoicesFiltersAndCompactsPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		items := []string{}
		if page == 1 {
			items = append(items, `{"id":1,"card_number":"0001","available_amount":1,"status":"ACTIVE","product_code":"test","archived_at":"2026-09-19T00:00:00Z"}`)
			for id := 2; id <= 20; id++ {
				items = append(items, fmt.Sprintf(`{"id":%d,"card_number":"%04d","available_amount":1,"status":"ACTIVE","product_code":"test"}`, id, id))
			}
		} else if page == 2 {
			for id := 21; id <= 22; id++ {
				items = append(items, fmt.Sprintf(`{"id":%d,"card_number":"%04d","available_amount":1,"status":"ACTIVE","product_code":"test"}`, id, id))
			}
		}
		fmt.Fprintf(w, `{"code":0,"data":{"total":22,"list":[%s]}}`, strings.Join(items, ","))
	}))
	defer srv.Close()

	client := New(Config{SiteBase: srv.URL, APIKey: "test-only"})
	first, total, err := client.UnarchivedCardChoices(context.Background(), 1)
	if err != nil || total != 21 || len(first) != 20 || first[0].ID != 2 || first[19].ID != 21 {
		t.Fatalf("bad compacted first page: total=%d list=%v err=%v", total, first, err)
	}
	second, total, err := client.UnarchivedCardChoices(context.Background(), 2)
	if err != nil || total != 21 || len(second) != 1 || second[0].ID != 22 {
		t.Fatalf("bad compacted second page: total=%d list=%v err=%v", total, second, err)
	}
	raw, _ := json.Marshal(first)
	if strings.Contains(string(raw), "archived_at") {
		t.Fatal("archive metadata escaped the server boundary")
	}
}

func TestCardChoicesRejectMalformedData(t *testing.T) {
	for _, body := range []string{`{}`, `{"total":0}`, `{"total":-1,"list":[]}`, `{"total":1,"list":[{"id":0}]}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"code":0,"data":` + body + `}`)) }))
		_, _, e := New(Config{SiteBase: srv.URL}).CardChoices(context.Background(), 1)
		srv.Close()
		if e == nil {
			t.Fatal("malformed data accepted", body)
		}
	}
}
