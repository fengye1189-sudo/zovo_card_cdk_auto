package cardplatform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetVIPAndCardUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "sk_test" {
			t.Fatal("missing api key")
		}
		switch r.URL.Path {
		case "/openapi/v1/vip":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"tier":"super","tier_name":"超级SVIP","active_cards":12,"cumulative_recharge":3456.78,"recharge_fee_rate":0.01}}`))
		case "/openapi/v1/gpt-direct/cards/42/usage":
			if r.URL.Query().Get("product") != "gpt" {
				t.Fatal("missing product")
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"card_id":42,"product":"gpt","success_count":3,"failure_count":1,"in_flight":1,"remaining":2,"max_uses":7,"cooldown_until":"2026-09-28T00:00:00Z"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(Config{SiteBase: server.URL, APIKey: "sk_test"})
	vip, err := client.GetVIP(context.Background())
	if err != nil || vip.Tier != "super" || vip.RechargeFeeRate == nil || *vip.RechargeFeeRate != 0.01 {
		t.Fatalf("unexpected vip: %#v %v", vip, err)
	}
	usage, err := client.GetCardUsage(context.Background(), 42, "gpt")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Successes != 3 || usage.Failures != 1 || usage.InFlight != 1 || usage.Used != 5 || usage.Remaining != 2 || usage.Limit != 7 {
		t.Fatalf("unexpected usage: %#v", usage)
	}
}

func TestGetCardUsageRejectsMismatchedCard(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"card_id":99,"product":"gpt","remaining":1}}`))
	}))
	defer server.Close()

	_, err := New(Config{SiteBase: server.URL, APIKey: "sk_test"}).GetCardUsage(context.Background(), 42, "gpt")
	if err == nil {
		t.Fatal("expected mismatch rejection")
	}
}
