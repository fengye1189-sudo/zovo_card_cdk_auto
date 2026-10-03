package jzactivation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultBaseURL = "https://apiai.jzplus.org"

type Config struct {
	BaseURL string
	APIKey  string
	Enabled bool
}

func LoadConfig() Config {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("JZ_ACTIVATION_API_BASE")), "/")
	if base == "" {
		base = defaultBaseURL
	}
	enabled := true
	if raw := strings.TrimSpace(os.Getenv("JZ_ACTIVATION_ENABLED")); raw != "" {
		if parsed, err := strconv.ParseBool(raw); err == nil {
			enabled = parsed
		}
	}
	return Config{
		BaseURL: strings.TrimSuffix(base, "/api/v1"),
		APIKey:  strings.TrimSpace(os.Getenv("JZ_ACTIVATION_API_KEY")),
		Enabled: enabled,
	}
}

type Client struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) *Client {
	return &Client{cfg: cfg, client: &http.Client{Timeout: 25 * time.Second}}
}

func NewFromEnv() *Client { return New(LoadConfig()) }

type ResponseError struct {
	HTTPStatus int
	Code       string
	Message    string
	RetryAfter int
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("activation api: http=%d code=%s message=%s", e.HTTPStatus, e.Code, e.Message)
}

type VerifyResult struct {
	Valid            bool   `json:"valid"`
	PlanType         string `json:"plan_type"`
	RefreshRemaining *int   `json:"refresh_remaining,omitempty"`
	Pending          bool   `json:"pending,omitempty"`
	Cancellable      bool   `json:"cancellable,omitempty"`
	Code             string `json:"code,omitempty"`
	Error            string `json:"error,omitempty"`
}

type SubscriptionSummary struct {
	AccountEmail                  string  `json:"account_email"`
	PlanType                      string  `json:"plan_type"`
	SubscriptionPlan              string  `json:"subscription_plan"`
	HasActiveSubscription         bool    `json:"has_active_subscription"`
	ExpiresAt                     string  `json:"expires_at"`
	RenewsAt                      string  `json:"renews_at"`
	CancelsAt                     *string `json:"cancels_at"`
	BillingCurrency               string  `json:"billing_currency"`
	BillingPeriod                 string  `json:"billing_period"`
	PurchaseOriginPlatform        string  `json:"purchase_origin_platform"`
	WillRenew                     bool    `json:"will_renew"`
	SubscriptionCancelledDetected bool    `json:"subscription_cancelled_detected"`
}

type SubscriptionResult struct {
	OK      bool                `json:"ok"`
	Summary SubscriptionSummary `json:"summary"`
	Code    string              `json:"code,omitempty"`
	Error   string              `json:"error,omitempty"`
}

type CreateTaskResult struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
	Error   string `json:"error,omitempty"`
}

type AnnouncementResult struct {
	Enabled   bool   `json:"enabled"`
	Content   string `json:"content"`
	UpdatedAt string `json:"updated_at"`
}

type QueueStatusResult struct {
	Status       string `json:"status"`
	PendingCount int    `json:"pending_count"`
	At           string `json:"at"`
}

type Challenge struct {
	Type           string `json:"type"`
	ClientSecret   string `json:"client_secret"`
	PublishableKey string `json:"publishable_key"`
}

type BatchTaskItem struct {
	CDKCode     string `json:"cdk_code"`
	SessionJSON string `json:"session_json"`
	Ref         string `json:"ref,omitempty"`
}

type BatchTaskResultItem struct {
	Index      int    `json:"index"`
	Ref        string `json:"ref,omitempty"`
	CDKCode    string `json:"cdk_code"`
	Status     string `json:"status"`
	TaskID     string `json:"task_id,omitempty"`
	TaskStatus string `json:"task_status,omitempty"`
	Code       string `json:"code,omitempty"`
	Error      string `json:"error,omitempty"`
}

type BatchTaskResult struct {
	BatchID   string                `json:"batch_id"`
	Accepted  int                   `json:"accepted"`
	Rejected  int                   `json:"rejected"`
	Conflict  int                   `json:"conflict"`
	Results   []BatchTaskResultItem `json:"results"`
}

type BatchLookupResult struct {
	Tasks []TaskResult `json:"tasks"`
}

type SimpleTaskResult struct {
	OK     bool   `json:"ok"`
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
	Error  string `json:"error,omitempty"`
}

type RefreshResult struct {
	Message          string `json:"message"`
	OldCode          string `json:"old_code"`
	NewCode          string `json:"new_code"`
	PlanType         string `json:"plan_type"`
	RefreshRemaining *int   `json:"refresh_remaining,omitempty"`
}

type TaskResult struct {
	TaskID        string `json:"task_id"`
	CDKCode       string `json:"cdk_code"`
	PlanType      string `json:"plan_type"`
	AccountEmail  string `json:"account_email"`
	TaskStatus    string `json:"task_status"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	CompletedAt   string `json:"completed_at,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
	Challenge     *Challenge `json:"challenge,omitempty"`
	Code          string `json:"code,omitempty"`
	Error         string `json:"error,omitempty"`
}

func (c *Client) Ping(ctx context.Context) (map[string]any, int, error) {
	var out map[string]any
	status, err := c.do(ctx, http.MethodGet, "", nil, &out)
	return out, status, err
}

func (c *Client) Announcement(ctx context.Context) (AnnouncementResult, int, error) {
	var out AnnouncementResult
	status, err := c.do(ctx, http.MethodGet, "/announcement", nil, &out)
	return out, status, err
}

func (c *Client) Verify(ctx context.Context, code string) (VerifyResult, int, error) {
	var out VerifyResult
	status, err := c.do(ctx, http.MethodPost, "/recharge/verify-cdk", map[string]string{"cdk_code": code}, &out)
	return out, status, err
}

func (c *Client) CheckSubscription(ctx context.Context, session string) (SubscriptionResult, int, error) {
	var out SubscriptionResult
	status, err := c.do(ctx, http.MethodPost, "/recharge/check-subscription", map[string]string{"token_input": session}, &out)
	return out, status, err
}

func (c *Client) CreateTask(ctx context.Context, code, session string) (CreateTaskResult, int, error) {
	var out CreateTaskResult
	status, err := c.do(ctx, http.MethodPost, "/recharge/create-task", map[string]string{
		"cdk_code": code, "session_json": session,
	}, &out)
	return out, status, err
}

func (c *Client) LookupTask(ctx context.Context, code string) (TaskResult, int, error) {
	var out TaskResult
	status, err := c.do(ctx, http.MethodGet, "/lookup/task?cdk_code="+url.QueryEscape(code), nil, &out)
	return out, status, err
}

func (c *Client) RefreshCDK(ctx context.Context, code string) (RefreshResult, int, error) {
	var out RefreshResult
	status, err := c.do(ctx, http.MethodPost, "/recharge/refresh-cdk", map[string]string{"cdk_code": code}, &out)
	return out, status, err
}

func (c *Client) CancelTask(ctx context.Context, code string) (SimpleTaskResult, int, error) {
	var out SimpleTaskResult
	status, err := c.do(ctx, http.MethodPost, "/recharge/cancel-task", map[string]string{"cdk_code": code}, &out)
	return out, status, err
}

func (c *Client) ResolveChallenge(ctx context.Context, code, taskID string) (SimpleTaskResult, int, error) {
	var out SimpleTaskResult
	status, err := c.do(ctx, http.MethodPost, "/recharge/challenge-resolved", map[string]string{"cdk_code": code, "task_id": taskID}, &out)
	return out, status, err
}

func (c *Client) CreateTasks(ctx context.Context, items []BatchTaskItem, idempotencyKey string) (BatchTaskResult, int, error) {
	var out BatchTaskResult
	headers := map[string]string{}
	if strings.TrimSpace(idempotencyKey) != "" {
		headers["Idempotency-Key"] = strings.TrimSpace(idempotencyKey)
	}
	status, err := c.doWithHeaders(ctx, http.MethodPost, "/partner/tasks/batch", map[string]any{"items": items}, &out, headers)
	return out, status, err
}

func (c *Client) LookupTasks(ctx context.Context, codes []string) (BatchLookupResult, int, error) {
	var out BatchLookupResult
	status, err := c.do(ctx, http.MethodPost, "/lookup/tasks", map[string]any{"codes": codes}, &out)
	return out, status, err
}

func (c *Client) QueueStatus(ctx context.Context) (QueueStatusResult, int, error) {
	var out QueueStatusResult
	status, err := c.do(ctx, http.MethodGet, "/recharge/queue-status", nil, &out)
	return out, status, err
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	return c.doWithHeaders(ctx, method, path, body, out, nil)
}

func (c *Client) doWithHeaders(ctx context.Context, method, path string, body any, out any, headers map[string]string) (int, error) {
	if !c.cfg.Enabled {
		return 0, &ResponseError{HTTPStatus: http.StatusServiceUnavailable, Code: "channel_disabled", Message: "channel disabled"}
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+"/api/v1"+path, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cfg.APIKey != "" {
		req.Header.Set("X-API-Key", c.cfg.APIKey)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if len(raw) > 0 && json.Unmarshal(raw, out) != nil {
		return resp.StatusCode, &ResponseError{HTTPStatus: resp.StatusCode, Code: "invalid_response", Message: "invalid JSON response"}
	}
	if resp.StatusCode >= 400 {
		var payload struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &payload)
		retryAfter, _ := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After")))
		return resp.StatusCode, &ResponseError{
			HTTPStatus: resp.StatusCode, Code: payload.Code,
			Message: payload.Error, RetryAfter: retryAfter,
		}
	}
	return resp.StatusCode, nil
}
