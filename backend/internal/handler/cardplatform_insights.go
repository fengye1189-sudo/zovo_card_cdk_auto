package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func InitCardPlatformInsights() error {
	_, err := db.DB.Exec(`CREATE TABLE IF NOT EXISTS cardplatform_account_insights(
 scope TEXT PRIMARY KEY,tier TEXT NOT NULL DEFAULT '',tier_name TEXT NOT NULL DEFAULT '',
 active_cards INTEGER NOT NULL DEFAULT 0,cumulative_recharge REAL NOT NULL DEFAULT 0,
 recharge_fee_rate REAL,next_card_page INTEGER NOT NULL DEFAULT 1,
 checked_at INTEGER NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS cardplatform_card_usage(
 scope TEXT NOT NULL,card_id INTEGER NOT NULL,product TEXT NOT NULL,
 product_code TEXT NOT NULL DEFAULT '',last4 TEXT NOT NULL DEFAULT '',card_status TEXT NOT NULL DEFAULT '',
 successes INTEGER NOT NULL DEFAULT 0,failures INTEGER NOT NULL DEFAULT 0,in_flight INTEGER NOT NULL DEFAULT 0,
 used_count INTEGER NOT NULL DEFAULT 0,usage_limit INTEGER NOT NULL DEFAULT 0,remaining INTEGER NOT NULL DEFAULT 0,
 cooldown_until TEXT NOT NULL DEFAULT '',open_fee REAL,recharge_fee_rate REAL,
 checked_at INTEGER NOT NULL,snapshot TEXT NOT NULL DEFAULT '{}',
 PRIMARY KEY(scope,card_id,product)
);
CREATE INDEX IF NOT EXISTS idx_cardplatform_usage_checked ON cardplatform_card_usage(scope,checked_at DESC);
CREATE TABLE IF NOT EXISTS cardplatform_cost_events(
 scope TEXT NOT NULL,event_id TEXT NOT NULL,event_type TEXT NOT NULL,kind TEXT NOT NULL DEFAULT '',
 card_id INTEGER NOT NULL DEFAULT 0,order_id INTEGER NOT NULL DEFAULT 0,auth_id TEXT NOT NULL DEFAULT '',
 currency TEXT NOT NULL DEFAULT '',ledger_amount_minor INTEGER,attribution_amount_minor INTEGER,
 direction TEXT NOT NULL DEFAULT '',status TEXT NOT NULL DEFAULT '',occurred_at TEXT NOT NULL DEFAULT '',
 recorded_at INTEGER NOT NULL,PRIMARY KEY(scope,event_id)
);
CREATE INDEX IF NOT EXISTS idx_cardplatform_cost_recent ON cardplatform_cost_events(scope,recorded_at DESC);`)
	return err
}

func insightScope() string { return financeScope() }

func safeMinor(value any) *int64 {
	var number float64
	switch v := value.(type) {
	case float64:
		number = v
	case json.Number:
		parsed, err := v.Float64()
		if err != nil {
			return nil
		}
		number = parsed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return nil
		}
		number = parsed
	default:
		return nil
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || math.Abs(number) > 1e9 {
		return nil
	}
	minor := math.Round(number * 100)
	if math.Abs(number*100-minor) > 0.000001 {
		return nil
	}
	result := int64(minor)
	return &result
}

func syncCardPlatformInsights(ctx context.Context) error {
	cfg := cardplatform.LoadConfig()
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil
	}
	client := cardplatform.New(cfg)
	scope := insightScope()
	now := time.Now().Unix()

	vip, err := client.GetVIP(ctx)
	if err != nil {
		_, _ = db.DB.ExecContext(ctx, `INSERT INTO cardplatform_account_insights(scope,last_error,checked_at)
 VALUES(?,?,?) ON CONFLICT(scope) DO UPDATE SET last_error=excluded.last_error,checked_at=excluded.checked_at`, scope, "会员等级和费率同步失败", now)
		return err
	}
	_, err = db.DB.ExecContext(ctx, `INSERT INTO cardplatform_account_insights(scope,tier,tier_name,active_cards,cumulative_recharge,recharge_fee_rate,checked_at,last_error)
 VALUES(?,?,?,?,?,?,?,'') ON CONFLICT(scope) DO UPDATE SET tier=excluded.tier,tier_name=excluded.tier_name,
 active_cards=excluded.active_cards,cumulative_recharge=excluded.cumulative_recharge,recharge_fee_rate=excluded.recharge_fee_rate,checked_at=excluded.checked_at,last_error=''`,
		scope, vip.Tier, vip.TierName, vip.ActiveCards, vip.CumulativeRecharge, vip.RechargeFeeRate, now)
	if err != nil {
		return err
	}

	products, err := client.AutomationProducts(ctx)
	if err != nil {
		return err
	}
	productMap := make(map[string]cardplatform.AutomationProduct, len(products))
	for _, product := range products {
		productMap[product.Code] = product
	}
	page := 1
	_ = db.DB.QueryRowContext(ctx, "SELECT next_card_page FROM cardplatform_account_insights WHERE scope=?", scope).Scan(&page)
	if page < 1 {
		page = 1
	}
	cards, total, err := client.CardChoices(ctx, page)
	if err != nil {
		return err
	}
	for _, card := range cards {
		usage, usageErr := client.GetCardUsage(ctx, card.ID, "gpt")
		if usageErr != nil {
			continue
		}
		snapshot, _ := json.Marshal(usage)
		var openFee any
		var rechargeFee any
		if product, ok := productMap[card.Product]; ok {
			openFee = product.OpenFee
			rechargeFee = product.RechargeFee
		}
		_, _ = db.DB.ExecContext(ctx, `INSERT INTO cardplatform_card_usage(
 scope,card_id,product,product_code,last4,card_status,successes,failures,in_flight,used_count,usage_limit,remaining,cooldown_until,open_fee,recharge_fee_rate,checked_at,snapshot)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(scope,card_id,product) DO UPDATE SET
 product_code=excluded.product_code,last4=excluded.last4,card_status=excluded.card_status,successes=excluded.successes,
 failures=excluded.failures,in_flight=excluded.in_flight,used_count=excluded.used_count,usage_limit=excluded.usage_limit,
 remaining=excluded.remaining,cooldown_until=excluded.cooldown_until,open_fee=excluded.open_fee,
 recharge_fee_rate=excluded.recharge_fee_rate,checked_at=excluded.checked_at,snapshot=excluded.snapshot`,
			scope, card.ID, "gpt", card.Product, card.Last4, card.Status, usage.Successes, usage.Failures,
			usage.InFlight, usage.Used, usage.Limit, usage.Remaining, usage.CooldownUntil, openFee, rechargeFee, now, string(snapshot))
	}
	nextPage := page + 1
	if page*20 >= total || len(cards) == 0 {
		nextPage = 1
	}
	_, err = db.DB.ExecContext(ctx, "UPDATE cardplatform_account_insights SET next_card_page=? WHERE scope=?", nextPage, scope)
	return err
}

func StartCardPlatformInsights(ctx context.Context) {
	go func() {
		timer := time.NewTimer(25 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				check, cancel := context.WithTimeout(ctx, 90*time.Second)
				_ = syncCardPlatformInsights(check)
				cancel()
				timer.Reset(15 * time.Minute)
			}
		}
	}()
}

func AdminCardPlatformInsights(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if c.Query("refresh") == "1" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
		err := syncCardPlatformInsights(ctx)
		cancel()
		if err != nil {
			localError(c, 502, "Zovo费率或卡容量暂时无法刷新，后台会自动重试")
			return
		}
	}
	scope := insightScope()
	account := gin.H{}
	var tier, tierName, lastError string
	var activeCards, checkedAt int64
	var cumulative float64
	var fee *float64
	if err := db.DB.QueryRow(`SELECT tier,tier_name,active_cards,cumulative_recharge,recharge_fee_rate,checked_at,last_error
 FROM cardplatform_account_insights WHERE scope=?`, scope).Scan(&tier, &tierName, &activeCards, &cumulative, &fee, &checkedAt, &lastError); err == nil {
		account = gin.H{"tier": tier, "tier_name": tierName, "active_cards": activeCards, "cumulative_recharge": cumulative, "recharge_fee_rate": fee, "checked_at": checkedAt, "last_error": lastError}
	}
	rows, err := db.DB.Query(`SELECT card_id,product_code,last4,card_status,successes,failures,in_flight,used_count,usage_limit,remaining,cooldown_until,open_fee,recharge_fee_rate,checked_at
 FROM cardplatform_card_usage WHERE scope=? ORDER BY checked_at DESC,card_id DESC LIMIT 200`, scope)
	if err != nil {
		localError(c, 503, "卡容量数据暂不可用")
		return
	}
	defer rows.Close()
	usage := []gin.H{}
	for rows.Next() {
		var cardID, successes, failures, inFlight, used, limit, remaining, at int64
		var productCode, last4, status, cooldown string
		var openFee, rechargeFee *float64
		if rows.Scan(&cardID, &productCode, &last4, &status, &successes, &failures, &inFlight, &used, &limit, &remaining, &cooldown, &openFee, &rechargeFee, &at) != nil {
			continue
		}
		allocatedOpenFee := 0.0
		if openFee != nil {
			divisor := successes
			if divisor < 1 {
				divisor = 1
			}
			allocatedOpenFee = *openFee / float64(divisor)
		}
		usage = append(usage, gin.H{"card_id": cardID, "product_code": productCode, "last4": last4, "status": status,
			"successes": successes, "failures": failures, "in_flight": inFlight, "used": used, "limit": limit, "remaining": remaining,
			"cooldown_until": cooldown, "open_fee": openFee, "allocated_open_fee": allocatedOpenFee, "recharge_fee_rate": rechargeFee, "checked_at": at})
	}
	var eventCount int64
	_ = db.DB.QueryRow("SELECT COUNT(*) FROM cardplatform_cost_events WHERE scope=?", scope).Scan(&eventCount)
	c.JSON(200, gin.H{"account": account, "cards": usage, "cost_event_count": eventCount})
}

func recordCardPlatformCostEvent(payload map[string]interface{}, eventType string) {
	if payload == nil || db.DB == nil {
		return
	}
	eventType = strings.ToLower(strings.TrimSpace(eventType))
	if eventType == "" {
		return
	}
	stringValue := func(key string) string {
		if value, ok := payload[key].(string); ok {
			return strings.TrimSpace(value)
		}
		return ""
	}
	intValue := func(key string) int64 {
		switch value := payload[key].(type) {
		case float64:
			return int64(value)
		case json.Number:
			result, _ := value.Int64()
			return result
		case string:
			result, _ := strconv.ParseInt(value, 10, 64)
			return result
		}
		return 0
	}
	eventID := stringValue("event_id")
	switch eventType {
	case "balance_change":
		eventID = fmt.Sprintf("balance:%d", intValue("balance_log_id"))
	case "card_fee":
		eventID = "fee:" + stringValue("auth_id") + ":" + stringValue("fee_type") + ":" + stringValue("occurred_at")
	case "card_transaction":
		eventID = "transaction:" + stringValue("auth_id") + ":" + stringValue("type") + ":" + stringValue("status")
	case "card_operation":
		eventID = "operation:" + stringValue("operation") + ":" + stringValue("operation_id") + ":" + stringValue("status")
	}
	if eventID == "" || strings.HasSuffix(eventID, ":0") {
		return
	}
	kind := stringValue("type")
	if eventType == "card_fee" {
		kind = stringValue("fee_type")
	} else if eventType == "card_operation" {
		kind = stringValue("operation")
	}
	var ledgerAmount *int64
	var attributionAmount *int64
	if eventType == "balance_change" {
		ledgerAmount = safeMinor(payload["amount"])
	} else if eventType == "card_fee" {
		attributionAmount = safeMinor(payload["amount"])
		if attributionAmount != nil && strings.EqualFold(stringValue("direction"), "credit") {
			negative := -*attributionAmount
			attributionAmount = &negative
		}
	}
	orderID := intValue("order_id")
	if orderID == 0 {
		// balance_change and some fee callbacks link the business order as ref_id.
		orderID = intValue("ref_id")
	}
	_, _ = db.DB.Exec(`INSERT INTO cardplatform_cost_events(scope,event_id,event_type,kind,card_id,order_id,auth_id,currency,ledger_amount_minor,attribution_amount_minor,direction,status,occurred_at,recorded_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(scope,event_id) DO UPDATE SET kind=excluded.kind,card_id=excluded.card_id,order_id=excluded.order_id,
 auth_id=excluded.auth_id,currency=excluded.currency,ledger_amount_minor=excluded.ledger_amount_minor,attribution_amount_minor=excluded.attribution_amount_minor,
 direction=excluded.direction,status=excluded.status,occurred_at=excluded.occurred_at,recorded_at=excluded.recorded_at`,
		insightScope(), eventID, eventType, kind, intValue("card_id"), orderID, stringValue("auth_id"), stringValue("currency"),
		ledgerAmount, attributionAmount, stringValue("direction"), stringValue("status"), stringValue("occurred_at"), time.Now().Unix())
}
