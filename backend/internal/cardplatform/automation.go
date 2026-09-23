package cardplatform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

func (c *Client) FindDirectOrder(ctx context.Context, request string, page int) (int64, int, error) {
	if request == "" || page < 1 || page > 10000 {
		return 0, 0, fmt.Errorf("invalid search")
	}
	raw, e := c.doOpenAPI(ctx, http.MethodGet, fmt.Sprintf("/gpt-direct/orders?page=%d&page_size=20", page), nil, "")
	if e != nil {
		return 0, 0, e
	}
	var v struct {
		Total *int `json:"total"`
		List  []struct {
			ID      int64  `json:"id"`
			Request string `json:"client_request_id"`
		} `json:"list"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Total == nil || v.List == nil {
		return 0, 0, fmt.Errorf("invalid list")
	}
	found := int64(0)
	for _, o := range v.List {
		if o.Request == request {
			if found != 0 || o.ID <= 0 {
				return 0, 0, fmt.Errorf("ambiguous match")
			}
			found = o.ID
		}
	}
	return found, *v.Total, nil
}

type AutomationProduct struct {
	Code        string    `json:"product_code"`
	Bin         string    `json:"bin"`
	Issuer      string    `json:"issuer"`
	Enabled     *bool     `json:"enabled"`
	OpenFee     *float64  `json:"open_fee"`
	RechargeFee *float64  `json:"recharge_fee"`
	Min         *float64  `json:"min_amount"`
	Max         *float64  `json:"max_amount"`
	Restricted  *[]string `json:"restricted_merchants"`
}

func (c *Client) AutomationProducts(ctx context.Context) ([]AutomationProduct, error) {
	raw, e := c.doOpenAPI(ctx, http.MethodGet, "/products", nil, "")
	if e != nil {
		return nil, e
	}
	var products []AutomationProduct
	if json.Unmarshal(raw, &products) != nil || products == nil {
		return nil, fmt.Errorf("invalid products")
	}
	return products, nil
}
func (c *Client) AutomationSpendable(ctx context.Context) (*float64, error) {
	raw, e := c.doOpenAPI(ctx, http.MethodGet, "/balance", nil, "")
	if e != nil {
		return nil, e
	}
	var v struct {
		Spendable *float64 `json:"spendable_balance"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Spendable == nil {
		return nil, fmt.Errorf("missing spendable balance")
	}
	return v.Spendable, nil
}
func (c *Client) AutomationRecharge(ctx context.Context, cardID, minor int64, key string) error {
	if cardID <= 0 || minor <= 0 || key == "" {
		return fmt.Errorf("invalid recharge")
	}
	_, e := c.doOpenAPI(ctx, http.MethodPost, "/cards/recharge", map[string]any{"card_id": cardID, "amount": float64(minor) / 100}, key)
	return e
}
func (c *Client) AutomationOpen(ctx context.Context, product, first, last string, minor int64, key string) (int64, error) {
	if product == "" || first == "" || last == "" || minor <= 0 || key == "" {
		return 0, fmt.Errorf("invalid opening")
	}
	raw, e := c.doOpenAPI(ctx, http.MethodPost, "/cards/open", map[string]any{"product_code": product, "first_name": first, "last_name": last, "init_amount": float64(minor) / 100}, key)
	if e != nil {
		return 0, e
	}
	// Do not persist or forward PAN, CVV, expiry or names from the opening response.
	var v struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal(raw, &v) != nil || v.ID <= 0 {
		return 0, fmt.Errorf("opening result unknown")
	}
	return v.ID, nil
}

// AutomationDeleteCard permanently closes one card. The upstream returns its
// remaining card balance to the platform spendable balance. A stable key lets
// the upstream deduplicate a repeated HTTP delivery without authorizing a
// second logical deletion.
func (c *Client) AutomationDeleteCard(ctx context.Context, cardID int64, key string) error {
	if cardID <= 0 || key == "" {
		return fmt.Errorf("invalid deletion")
	}
	_, e := c.doOpenAPI(ctx, http.MethodDelete, fmt.Sprintf("/cards/%d", cardID), nil, key)
	return e
}
