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

const MaxCardRemarkRunes = 1200

type CardChoice struct {
	ID         int64    `json:"id"`
	Last4      string   `json:"last4"`
	Product    string   `json:"product_code"`
	Status     string   `json:"status"`
	Balance    *float64 `json:"balance_usd"`
	ArchivedAt string   `json:"-"`
	// Remark may contain an administrator's private note. It is used only by
	// server-side synchronization and must never cross our JSON boundary.
	Remark string `json:"-"`
}

// Only masked, allowlisted fields may leave the server. Never fetch card details.
func (c *Client) CardChoices(ctx context.Context, page int) ([]CardChoice, int, error) {
	return c.cardChoices(ctx, page, false)
}
func (c *Client) CardChoicesSync(ctx context.Context, page int) ([]CardChoice, int, error) {
	return c.cardChoices(ctx, page, true)
}

// UnarchivedCardChoices compacts upstream pages after excluding archived cards.
// The upstream may still report an archived card as ACTIVE, so status alone is
// not authoritative for the administrator's selectable-card list.
func (c *Client) UnarchivedCardChoices(ctx context.Context, page int) ([]CardChoice, int, error) {
	if page < 1 || page > 10000 {
		return nil, 0, fmt.Errorf("invalid page")
	}
	visible := []CardChoice{}
	for sourcePage := 1; sourcePage <= 10000; sourcePage++ {
		cards, total, err := c.cardChoices(ctx, sourcePage, false)
		if err != nil {
			return nil, 0, err
		}
		for _, card := range cards {
			if card.ArchivedAt == "" {
				visible = append(visible, card)
			}
		}
		if sourcePage*20 >= total {
			start := (page - 1) * 20
			if start >= len(visible) {
				return []CardChoice{}, len(visible), nil
			}
			end := start + 20
			if end > len(visible) {
				end = len(visible)
			}
			return visible[start:end], len(visible), nil
		}
	}
	return nil, 0, fmt.Errorf("card list pagination did not finish")
}

func (c *Client) cardChoices(ctx context.Context, page int, sync bool) ([]CardChoice, int, error) {
	path := "/cards?page=" + strconv.Itoa(page) + "&page_size=20"
	if sync {
		path += "&sync=1"
	}
	raw, e := c.doOpenAPI(ctx, http.MethodGet, path, nil, "")
	if e != nil {
		return nil, 0, e
	}
	var response struct {
		Total *int `json:"total"`
		List  []struct {
			ID         int64    `json:"id"`
			Number     string   `json:"card_number"`
			Product    string   `json:"product_code"`
			Status     string   `json:"status"`
			Balance    *float64 `json:"available_amount"`
			ArchivedAt string   `json:"archived_at"`
			Remark     string   `json:"remark"`
		} `json:"list"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Total == nil || *response.Total < 0 || response.List == nil {
		return nil, 0, fmt.Errorf("invalid card list")
	}
	result := make([]CardChoice, 0, len(response.List))
	for _, v := range response.List {
		if v.ID <= 0 {
			return nil, 0, fmt.Errorf("invalid card id")
		}
		digits := ""
		for _, r := range v.Number {
			if r >= '0' && r <= '9' {
				digits += string(r)
			}
		}
		last := ""
		if len(digits) >= 4 {
			last = digits[len(digits)-4:]
		}
		result = append(result, CardChoice{ID: v.ID, Last4: last, Product: v.Product, Status: v.Status, Balance: v.Balance, ArchivedAt: v.ArchivedAt, Remark: v.Remark})
	}
	return result, *response.Total, nil
}

// SetCardRemark writes only the card's free-text remark. The operation is
// idempotent because every call sends the complete desired value.
func (c *Client) SetCardRemark(ctx context.Context, cardID int64, remark string) error {
	if cardID <= 0 {
		return fmt.Errorf("invalid card id")
	}
	remark = strings.TrimSpace(remark)
	if len([]rune(remark)) > MaxCardRemarkRunes {
		return fmt.Errorf("card remark exceeds %d characters", MaxCardRemarkRunes)
	}
	_, err := c.doOpenAPI(ctx, http.MethodPut, "/cards/"+url.PathEscape(strconv.FormatInt(cardID, 10))+"/remark", map[string]any{"remark": remark}, "")
	return err
}
