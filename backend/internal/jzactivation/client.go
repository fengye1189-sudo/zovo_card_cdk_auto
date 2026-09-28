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
	Code          string `json:"code,omitempty"`
	Error         string `json:"error,omitempty"`
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

func (c *Client) do(ctx context.Context, method, path string, body any, out any) (int, error) {
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
