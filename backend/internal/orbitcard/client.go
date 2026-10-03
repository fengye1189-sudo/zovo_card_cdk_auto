// Package orbitcard implements the server-side Orbitcard V1 clients.
// Secrets are read only from environment variables and are never included in
// errors or returned payloads.
package orbitcard

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

const defaultBaseURL = "https://orbitcard.cc"

type Config struct {
	BaseURL string
	APIKey string
	APISecret string
	Enabled bool
}

func LoadConfig() Config {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("ORBITCARD_BASE_URL")), "/")
	if base == "" { base = defaultBaseURL }
	enabled := strings.EqualFold(strings.TrimSpace(os.Getenv("ORBITCARD_ENABLED")), "true")
	return Config{
		BaseURL: base,
		APIKey: strings.TrimSpace(os.Getenv("ORBITCARD_API_KEY")),
		APISecret: strings.TrimSpace(os.Getenv("ORBITCARD_API_SECRET")),
		Enabled: enabled,
	}
}

type Client struct { cfg Config; http *http.Client }

func New(cfg Config) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}}
}

func NewFromEnv() *Client { return New(LoadConfig()) }

type APIError struct { HTTPStatus int; Code int; Message string }

func (e *APIError) Error() string { return fmt.Sprintf("orbitcard api: http=%d code=%d message=%s", e.HTTPStatus, e.Code, e.Message) }

type envelope struct { Code int `json:"code"`; Msg string `json:"msg"`; Data json.RawMessage `json:"data"` }

func nonce() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil { return "", err }
	return hex.EncodeToString(b), nil
}

func bodyHash(body []byte) string { sum := sha256.Sum256(body); return hex.EncodeToString(sum[:]) }

// CanonicalSignature is exported for deterministic tests and operational
// diagnostics. The returned value is only the hex digest, never the secret.
func CanonicalSignature(secret, apiKey string, timestamp int64, nonce, method, host, path, query string, body []byte, idem string) string {
	canonical := strings.Join([]string{
		"ORBITCARD-HMAC-SHA256-V1", apiKey, strconv.FormatInt(timestamp, 10), nonce,
		strings.ToUpper(method), host, path, query, bodyHash(body), idem,
	}, "\n")
	h := hmac.New(sha256.New, []byte(secret)); _, _ = h.Write([]byte(canonical))
	return hex.EncodeToString(h.Sum(nil))
}

func (c *Client) post(ctx context.Context, path string, payload any, idem string) (json.RawMessage, error) {
	if c == nil || !c.cfg.Enabled { return nil, &APIError{HTTPStatus: 503, Message: "orbitcard is disabled"} }
	if c.cfg.APIKey == "" || c.cfg.APISecret == "" { return nil, &APIError{HTTPStatus: 503, Message: "orbitcard credentials are not configured"} }
	body, err := json.Marshal(payload); if err != nil { return nil, err }
	u, err := url.Parse(strings.TrimRight(c.cfg.BaseURL, "/") + path); if err != nil { return nil, err }
	n, err := nonce(); if err != nil { return nil, err }
	ts := time.Now().Unix()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body)); if err != nil { return nil, err }
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.cfg.APIKey)
	req.Header.Set("X-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Nonce", n)
	req.Header.Set("X-Signature", CanonicalSignature(c.cfg.APISecret, c.cfg.APIKey, ts, n, http.MethodPost, u.Host, u.Path, u.RawQuery, body, idem))
	if idem != "" { req.Header.Set("Idempotency-Key", idem) }
	resp, err := c.http.Do(req); if err != nil { return nil, err }
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil { return nil, &APIError{HTTPStatus: resp.StatusCode, Message: "invalid orbitcard response"} }
	if resp.StatusCode >= 400 || env.Code != 0 { return nil, &APIError{HTTPStatus: resp.StatusCode, Code: env.Code, Message: env.Msg} }
	return env.Data, nil
}

func (c *Client) Plans(ctx context.Context) (json.RawMessage, error) { return c.post(ctx, "/api/open/v1/getSubscriptionPlans", map[string]any{}, "") }
func (c *Client) Regions(ctx context.Context, plan string) (json.RawMessage, error) { return c.post(ctx, "/api/open/v1/getSubscriptionPaymentRegions", map[string]string{"plan_type": plan}, "") }
func (c *Client) Balance(ctx context.Context) (json.RawMessage, error) { return c.post(ctx, "/api/open/v1/getAccountBalance", map[string]any{}, "") }

func (c *Client) CreateSubscription(ctx context.Context, body map[string]any, idem string) (json.RawMessage, error) {
	return c.post(ctx, "/api/open/v1/createSubscription", body, idem)
}

func (c *Client) GetSubscription(ctx context.Context, query map[string]string) (json.RawMessage, error) {
	return c.post(ctx, "/api/open/v1/getSubscription", query, "")
}
