package cardplatform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// VIPStatus is the effective account pricing tier returned by Zovo. Rates are
// decimals (for example 0.01 means 1%). Pointer fields keep a missing upstream
// value distinct from a real zero-fee tier.
type VIPStatus struct {
	Tier               string   `json:"tier"`
	TierName           string   `json:"tier_name"`
	ActiveCards        int64    `json:"active_cards"`
	CumulativeRecharge float64  `json:"cumulative_recharge"`
	RechargeFeeRate    *float64 `json:"recharge_fee_rate"`
}

func (c *Client) GetVIP(ctx context.Context) (*VIPStatus, error) {
	raw, err := c.doOpenAPI(ctx, http.MethodGet, "/vip", nil, "")
	if err != nil {
		return nil, err
	}
	var out VIPStatus
	if json.Unmarshal(raw, &out) != nil || strings.TrimSpace(out.Tier) == "" || out.ActiveCards < 0 || out.CumulativeRecharge < 0 {
		return nil, fmt.Errorf("invalid vip status")
	}
	if out.RechargeFeeRate != nil && (*out.RechargeFeeRate < 0 || *out.RechargeFeeRate > 1) {
		return nil, fmt.Errorf("invalid recharge fee rate")
	}
	return &out, nil
}

// CardUsage contains only the operational counters needed for capacity and
// cost allocation. The upstream has added fields over time, so aliases are
// accepted while unknown fields are ignored.
type CardUsage struct {
	CardID        int64  `json:"card_id"`
	Product       string `json:"product"`
	Plan          string `json:"plan,omitempty"`
	Successes     int64  `json:"successes"`
	Failures      int64  `json:"failures"`
	InFlight      int64  `json:"in_flight"`
	Used          int64  `json:"used"`
	Limit         int64  `json:"limit"`
	Remaining     int64  `json:"remaining"`
	CooldownUntil string `json:"cooldown_until,omitempty"`
}

func usageInt(m map[string]any, keys ...string) int64 {
	for _, key := range keys {
		value, ok := m[key]
		if !ok || value == nil {
			continue
		}
		switch v := value.(type) {
		case float64:
			return int64(v)
		case json.Number:
			n, _ := v.Int64()
			return n
		case string:
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}

func usageString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := m[key].(string); ok {
			return adminText(value)
		}
	}
	return ""
}

func (c *Client) GetCardUsage(ctx context.Context, cardID int64, product string) (*CardUsage, error) {
	if cardID <= 0 {
		return nil, fmt.Errorf("invalid card id")
	}
	product = strings.ToLower(strings.TrimSpace(product))
	if product == "" {
		product = "gpt"
	}
	if product != "gpt" && product != "claude" && product != "grok" {
		return nil, fmt.Errorf("unsupported product")
	}
	path := "/gpt-direct/cards/" + strconv.FormatInt(cardID, 10) + "/usage?product=" + url.QueryEscape(product)
	raw, err := c.doOpenAPI(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var values map[string]any
	if decoder.Decode(&values) != nil || values == nil {
		return nil, fmt.Errorf("invalid card usage")
	}
	result := &CardUsage{
		CardID:        usageInt(values, "card_id", "id"),
		Product:       usageString(values, "product"),
		Plan:          usageString(values, "plan"),
		Successes:     usageInt(values, "successes", "success_count", "completed_uses", "successful_uses"),
		Failures:      usageInt(values, "failures", "failure_count", "failed_uses"),
		InFlight:      usageInt(values, "in_flight", "inflight", "reserved", "pending_uses"),
		Used:          usageInt(values, "used", "used_count", "total_uses"),
		Limit:         usageInt(values, "limit", "max_uses", "usage_limit"),
		Remaining:     usageInt(values, "remaining", "remaining_uses"),
		CooldownUntil: usageString(values, "cooldown_until", "cooldown_end"),
	}
	if result.CardID == 0 {
		result.CardID = cardID
	}
	if result.CardID != cardID {
		return nil, fmt.Errorf("card usage mismatch")
	}
	if result.Product == "" {
		result.Product = product
	}
	if result.Product != product {
		return nil, fmt.Errorf("card usage product mismatch")
	}
	for _, n := range []int64{result.Successes, result.Failures, result.InFlight, result.Used, result.Limit} {
		if n < 0 {
			return nil, fmt.Errorf("invalid card usage counters")
		}
	}
	if result.Remaining < -1 {
		return nil, fmt.Errorf("invalid remaining usage")
	}
	if result.Used == 0 {
		result.Used = result.Successes + result.Failures + result.InFlight
	}
	return result, nil
}
