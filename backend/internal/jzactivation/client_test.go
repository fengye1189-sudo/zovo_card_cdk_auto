package jzactivation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientUsesServerSideKeyAndParsesFlow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "merchant-secret" {
			t.Fatalf("missing server-side API key")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/recharge/verify-cdk":
			_ = json.NewEncoder(w).Encode(map[string]any{"valid": true, "plan_type": "plus", "refresh_remaining": 3})
		case "/api/v1/recharge/check-subscription":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "summary": map[string]any{"account_email": "a@example.com", "plan_type": "free"}})
		case "/api/v1/recharge/create-task":
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "TASK-1", "status": "submitted"})
		case "/api/v1/lookup/task":
			if r.URL.Query().Get("cdk_code") != "CODE +&" {
				t.Fatalf("unexpected escaped code: %q", r.URL.Query().Get("cdk_code"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "TASK-1", "task_status": "completed"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL, APIKey: "merchant-secret", Enabled: true})
	verified, _, err := client.Verify(context.Background(), "CODE")
	if err != nil || !verified.Valid || verified.PlanType != "plus" {
		t.Fatalf("verify=%+v err=%v", verified, err)
	}
	checked, _, err := client.CheckSubscription(context.Background(), `{}`)
	if err != nil || !checked.OK || checked.Summary.AccountEmail != "a@example.com" {
		t.Fatalf("subscription=%+v err=%v", checked, err)
	}
	created, _, err := client.CreateTask(context.Background(), "CODE", `{}`)
	if err != nil || created.TaskID != "TASK-1" {
		t.Fatalf("create=%+v err=%v", created, err)
	}
	task, _, err := client.LookupTask(context.Background(), "CODE +&")
	if err != nil || task.TaskStatus != "completed" {
		t.Fatalf("task=%+v err=%v", task, err)
	}
}

func TestClientReturnsStructuredProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"rate_limited","error":"请稍后再试"}`))
	}))
	defer server.Close()
	client := New(Config{BaseURL: server.URL, Enabled: true})
	_, _, err := client.Verify(context.Background(), "CODE")
	responseErr, ok := err.(*ResponseError)
	if !ok || responseErr.Code != "rate_limited" || responseErr.RetryAfter != 12 {
		t.Fatalf("error=%#v", err)
	}
}
