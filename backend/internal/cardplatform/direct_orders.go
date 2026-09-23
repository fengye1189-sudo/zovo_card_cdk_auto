package cardplatform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
)

type DirectCandidate struct {
	CardID       int64    `json:"card_id"`
	Skip         *bool    `json:"skip"`
	SkipReason   string   `json:"skip_reason"`
	AvailableUSD *float64 `json:"available_usd"`
	LightRemain  *int64   `json:"light_remain"`
}

// This endpoint previews existing cards only; it never opens, funds or pays.
func (c *Client) DirectCandidates(ctx context.Context) ([]DirectCandidate, error) {
	return c.DirectCandidatesForPlan(ctx, "plus")
}
func (c *Client) DirectCandidatesForPlan(ctx context.Context, plan string) ([]DirectCandidate, error) {
	if plan != "plus" && plan != "go" && plan != "pro_5x" && plan != "pro_20x" {
		return nil, fmt.Errorf("unsupported plan")
	}
	raw, err := c.doOpenAPI(ctx, http.MethodGet, "/gpt-direct/card-pool/schedule?product=gpt&plan="+plan+"&card_mode=auto_existing", nil, "")
	if err != nil {
		return nil, err
	}
	var v struct {
		Product    string            `json:"product"`
		Plan       string            `json:"plan"`
		Candidates []DirectCandidate `json:"candidates"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Product != "gpt" || v.Plan != plan || v.Candidates == nil {
		return nil, fmt.Errorf("invalid card pool")
	}
	seen := map[int64]bool{}
	for _, item := range v.Candidates {
		if item.CardID <= 0 || seen[item.CardID] {
			return nil, fmt.Errorf("ambiguous card pool")
		}
		seen[item.CardID] = true
	}
	return v.Candidates, nil
}

func (v DirectCandidate) Usable(minUSDMinor int64) bool {
	if minUSDMinor <= 0 || v.Skip == nil || *v.Skip || v.SkipReason != "" || v.AvailableUSD == nil || v.LightRemain == nil {
		return false
	}
	if *v.LightRemain != -1 && *v.LightRemain <= 0 {
		return false
	}
	balance := *v.AvailableUSD
	return !math.IsNaN(balance) && !math.IsInf(balance, 0) && balance >= 0 && math.Floor(balance*100+0.000001) >= float64(minUSDMinor)
}

// DirectPricing is deliberately stricter than the legacy display-only parser.
// Missing eligibility or price fields must never authorize a payment.
var ErrDirectPlanUnavailable = errors.New("direct plan unavailable")

func (c *Client) DirectPricing(ctx context.Context, plan string) (int64, int64, error) {
	raw, err := c.doOpenAPI(ctx, http.MethodGet, "/gpt-direct/plans?product=gpt", nil, "")
	if err != nil {
		return 0, 0, err
	}
	var v struct {
		Version int64 `json:"version"`
		Plans   map[string]struct {
			Enabled *bool  `json:"enabled"`
			Fee     *int64 `json:"serviceFeeUsdMinor"`
		} `json:"plans"`
		Registry []struct {
			Key         string `json:"key"`
			Product     string `json:"product"`
			AccPlanKey  string `json:"acc_plan_key"`
			Purchasable *bool  `json:"purchasable"`
		} `json:"registry"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Version <= 0 {
		return 0, 0, fmt.Errorf("invalid direct pricing")
	}
	matches := 0
	var fee int64
	for _, item := range v.Registry {
		if item.Key != plan || item.Product != "gpt" {
			continue
		}
		matches++
		p, exists := v.Plans[item.AccPlanKey]
		if item.AccPlanKey == "" || item.Purchasable == nil || !exists || p.Enabled == nil || p.Fee == nil || *p.Fee < 0 {
			return 0, 0, fmt.Errorf("incomplete direct pricing")
		}
		if !*item.Purchasable || !*p.Enabled {
			return 0, 0, ErrDirectPlanUnavailable
		}
		fee = *p.Fee
	}
	if matches != 1 {
		return 0, 0, fmt.Errorf("missing or ambiguous direct plan")
	}
	return v.Version, fee, nil
}

// DirectPreflight and DirectOrder do not issue or purchase an upstream CDK.
func (c *Client) DirectPreflight(ctx context.Context, body any) (json.RawMessage, error) {
	return c.doOpenAPI(ctx, http.MethodPost, "/gpt-direct/preflight", body, "")
}
func (c *Client) DirectOrder(ctx context.Context, body any, requestID string) (json.RawMessage, error) {
	return c.doOpenAPI(ctx, http.MethodPost, "/gpt-direct/orders", body, requestID)
}
func (c *Client) DirectOrderStatus(ctx context.Context, id int64) (json.RawMessage, error) {
	return c.doOpenAPI(ctx, http.MethodGet, "/gpt-direct/orders/"+strconv.FormatInt(id, 10), nil, "")
}
