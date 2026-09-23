package handler

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

type operationsCustomerSearch struct {
	Page       int    `json:"page"`
	PageSize   int    `json:"page_size"`
	Plan       string `json:"plan"`
	State      string `json:"state"`
	ExpiryDays int    `json:"expiry_days"`
	Query      string `json:"q"`
	Timezone   string `json:"timezone"`
	Export     bool   `json:"export"`
	Segment    string `json:"segment"`
}

type operationsCustomerPlanSummary struct {
	Plan          string `json:"plan"`
	ValidAccounts int64  `json:"valid_accounts"`
	NewToday      int64  `json:"new_today"`
	Expiring3Days int64  `json:"expiring_3d"`
}

type operationsCustomer struct {
	ID                    int64  `json:"id"`
	Email                 string `json:"email"`
	Plan                  string `json:"plan"`
	UpgradeType           string `json:"upgrade_type"`
	ActivatedAt           int64  `json:"activated_at"`
	SubscriptionExpiresAt int64  `json:"subscription_expires_at"`
	ExpiryEstimated       bool   `json:"expiry_estimated"`
	UpstreamID            int64  `json:"upstream_id"`
	CardID                int64  `json:"card_id"`
	MarketplaceOrderID    string `json:"marketplace_order_id"`
	BuyerName             string `json:"buyer_name"`
	BuyerEmail            string `json:"buyer_email"`
	TelegramID            string `json:"telegram_id"`
	TelegramUsername      string `json:"telegram_username"`
}

type marketplaceCustomerIdentity struct {
	OrderID          string `json:"orderId"`
	BuyerName        string `json:"buyerName"`
	BuyerEmail       string `json:"buyerEmail"`
	TelegramID       string `json:"telegramId"`
	TelegramUsername string `json:"telegramUsername"`
}

const operationsCustomerCurrentCTE = `WITH ranked_customers AS (
	SELECT l.id,LOWER(TRIM(l.email)) AS account_key,TRIM(l.email) AS email,l.plan,
	       l.upgrade_type,l.activated_at,l.subscription_expires_at,l.expiry_estimated,
	       l.upstream_id,l.card_id,COALESCE(m.marketplace_order_id,'') AS marketplace_order_id,
	       ROW_NUMBER() OVER (
	           PARTITION BY LOWER(TRIM(l.email))
	           ORDER BY l.activated_at DESC,l.id DESC
	       ) AS customer_rank
	FROM local_cdks l LEFT JOIN marketplace_local_cdk_bindings m ON m.local_id=l.id
	WHERE l.status='consumed' AND TRIM(l.email)<>''
	  AND l.activated_at>0 AND l.subscription_expires_at>0
), current_customers AS (
	SELECT id,account_key,email,plan,upgrade_type,activated_at,subscription_expires_at,
	       expiry_estimated,upstream_id,card_id,marketplace_order_id
	FROM ranked_customers WHERE customer_rank=1
) `

func operationsCustomerLocation(name string) (*time.Location, error) {
	switch strings.TrimSpace(name) {
	case "", "Asia/Bangkok":
		return time.FixedZone("Asia/Bangkok", 7*60*60), nil
	case "UTC":
		return time.UTC, nil
	default:
		return nil, fmt.Errorf("不支持的日期时区")
	}
}

func marketplaceCustomerIdentities(ctx *gin.Context, orderIDs []string) (map[string]marketplaceCustomerIdentity, error) {
	result := map[string]marketplaceCustomerIdentity{}
	if len(orderIDs) == 0 {
		return result, nil
	}
	body, err := json.Marshal(map[string]any{"v": 1, "orderIds": orderIDs})
	if err != nil {
		return nil, err
	}
	secret, err := completionBridgeSecret()
	if err != nil {
		return nil, err
	}
	stamp := fmt.Sprintf("%d", time.Now().UnixMilli())
	url := strings.TrimSpace(os.Getenv("CDK_CUSTOMER_IDENTITIES_URL"))
	if url == "" {
		url = "https://maple1189ai.com/api/internal/cdk-customer-identities"
	}
	req, err := http.NewRequestWithContext(ctx.Request.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MaplePass-Time", stamp)
	req.Header.Set("X-MaplePass-Signature", completionBridgeSignature(secret, "cdk-customer-identities:v1", stamp, body))
	response, err := marketplaceCompletionClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 256<<10))
	if err != nil || response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("customer identity lookup unavailable")
	}
	var payload struct {
		Identities []marketplaceCustomerIdentity `json:"identities"`
	}
	if err = json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	for _, item := range payload.Identities {
		if item.OrderID != "" {
			result[item.OrderID] = item
		}
	}
	return result, nil
}

func operationsCustomerPlanAllowed(plan string) bool {
	return plan == "" || plan == "plus" || plan == "go" || plan == "pro_5x" || plan == "pro_20x"
}

func operationsCustomerFilters(req operationsCustomerSearch, now, todayStart, tomorrowStart int64) (string, []any, error) {
	if !operationsCustomerPlanAllowed(req.Plan) {
		return "", nil, fmt.Errorf("套餐筛选不正确")
	}
	if req.State == "" {
		req.State = "active"
	}
	if req.State != "active" && req.State != "expired" && req.State != "all" {
		return "", nil, fmt.Errorf("客户状态筛选不正确")
	}
	if req.ExpiryDays != 0 && req.ExpiryDays != 3 && req.ExpiryDays != 7 && req.ExpiryDays != 15 {
		return "", nil, fmt.Errorf("到期范围只支持 3、7 或 15 天")
	}
	if req.Segment != "" && req.Segment != "active" && req.Segment != "new_today" && req.Segment != "expiring_3d" {
		return "", nil, fmt.Errorf("客户分组不正确")
	}
	q := strings.TrimSpace(req.Query)
	if len(q) > 254 {
		return "", nil, fmt.Errorf("搜索内容过长")
	}
	clauses := []string{"1=1"}
	args := []any{}
	if req.Plan != "" {
		clauses = append(clauses, "plan=?")
		args = append(args, req.Plan)
	}
	if req.Segment == "new_today" {
		clauses = append(clauses, "activated_at>=? AND activated_at<?")
		args = append(args, todayStart, tomorrowStart)
	} else if req.Segment == "expiring_3d" {
		clauses = append(clauses, "subscription_expires_at>? AND subscription_expires_at<=?")
		args = append(args, now, now+3*86400)
	}
	if req.Segment == "new_today" {
		// Today's upgrades can include an account whose subscription data has
		// just crossed a boundary; the segment itself is the authoritative filter.
	} else if req.Segment == "expiring_3d" {
		// Already constrained above.
	} else if req.ExpiryDays > 0 {
		clauses = append(clauses, "subscription_expires_at>? AND subscription_expires_at<=?")
		args = append(args, now, now+int64(req.ExpiryDays)*86400)
	} else if req.State == "active" {
		clauses = append(clauses, "subscription_expires_at>?")
		args = append(args, now)
	} else if req.State == "expired" {
		clauses = append(clauses, "subscription_expires_at<=?")
		args = append(args, now)
	}
	if q != "" {
		q = strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(strings.ToLower(q))
		clauses = append(clauses, `account_key LIKE ? ESCAPE '\'`)
		args = append(args, "%"+q+"%")
	}
	return " WHERE " + strings.Join(clauses, " AND "), args, nil
}

// OperationsCustomersSearch derives a retention view from authoritative local
// redemption completions. One latest successful row per normalized account is
// counted, so renewals and plan changes never inflate the active customer total.
func OperationsCustomersSearch(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var req operationsCustomerSearch
	if !localBody(c, &req) {
		return
	}
	if req.Page == 0 {
		req.Page = 1
	}
	if req.PageSize == 0 {
		req.PageSize = 25
	}
	if req.Export {
		req.Page = 1
		req.PageSize = 5000
	}
	if req.Page < 1 || req.Page > 100000000 || req.PageSize < 1 || req.PageSize > 5000 || (!req.Export && req.PageSize > 100) {
		localError(c, 400, "页码或每页数量不正确")
		return
	}
	location, err := operationsCustomerLocation(req.Timezone)
	if err != nil {
		localError(c, 400, err.Error())
		return
	}
	now := time.Now().Unix()
	nowAtLocation := time.Unix(now, 0).In(location)
	todayStart := time.Date(nowAtLocation.Year(), nowAtLocation.Month(), nowAtLocation.Day(), 0, 0, 0, 0, location).Unix()
	tomorrowStart := time.Unix(todayStart, 0).In(location).AddDate(0, 0, 1).Unix()
	where, args, err := operationsCustomerFilters(req, now, todayStart, tomorrowStart)
	if err != nil {
		localError(c, 400, err.Error())
		return
	}

	tx, err := db.DB.BeginTx(c.Request.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		localError(c, 503, "读取客户数据繁忙，请重试")
		return
	}
	defer tx.Rollback()

	planMap := map[string]*operationsCustomerPlanSummary{}
	for _, plan := range []string{"plus", "go", "pro_5x", "pro_20x"} {
		planMap[plan] = &operationsCustomerPlanSummary{Plan: plan}
	}
	rows, err := tx.Query(operationsCustomerCurrentCTE+`SELECT plan,
		SUM(CASE WHEN subscription_expires_at>? THEN 1 ELSE 0 END),
		SUM(CASE WHEN activated_at>=? AND activated_at<? THEN 1 ELSE 0 END),
		SUM(CASE WHEN subscription_expires_at>? AND subscription_expires_at<=? THEN 1 ELSE 0 END)
		FROM current_customers GROUP BY plan`, now, todayStart, tomorrowStart, now, now+3*86400)
	if err != nil {
		localError(c, 500, "读取客户统计失败")
		return
	}
	for rows.Next() {
		var plan string
		var valid, today, expiring int64
		if err = rows.Scan(&plan, &valid, &today, &expiring); err != nil {
			rows.Close()
			localError(c, 500, "读取客户统计失败")
			return
		}
		if item, ok := planMap[plan]; ok {
			item.ValidAccounts, item.NewToday, item.Expiring3Days = valid, today, expiring
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		localError(c, 500, "读取客户统计失败")
		return
	}
	rows.Close()
	plans := make([]operationsCustomerPlanSummary, 0, 4)
	totals := operationsCustomerPlanSummary{Plan: "all"}
	for _, plan := range []string{"plus", "go", "pro_5x", "pro_20x"} {
		item := *planMap[plan]
		plans = append(plans, item)
		totals.ValidAccounts += item.ValidAccounts
		totals.NewToday += item.NewToday
		totals.Expiring3Days += item.Expiring3Days
	}

	var completed, missingEmail, missingDates int64
	err = tx.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN TRIM(email)='' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN activated_at<=0 OR subscription_expires_at<=0 THEN 1 ELSE 0 END),0)
		FROM local_cdks WHERE status='consumed'`).Scan(&completed, &missingEmail, &missingDates)
	if err != nil {
		localError(c, 500, "读取客户数据质量失败")
		return
	}

	var total int64
	if err = tx.QueryRow(operationsCustomerCurrentCTE+"SELECT COUNT(*) FROM current_customers"+where, args...).Scan(&total); err != nil {
		localError(c, 500, "读取客户数量失败")
		return
	}
	queryArgs := append(append([]any{}, args...), req.PageSize, int64(req.Page-1)*int64(req.PageSize))
	rows, err = tx.Query(operationsCustomerCurrentCTE+`SELECT id,email,plan,upgrade_type,activated_at,
		subscription_expires_at,expiry_estimated,upstream_id,card_id,marketplace_order_id
		FROM current_customers`+where+` ORDER BY subscription_expires_at ASC,id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		localError(c, 500, "读取客户列表失败")
		return
	}
	list := []operationsCustomer{}
	for rows.Next() {
		var item operationsCustomer
		var estimated int
		if err = rows.Scan(&item.ID, &item.Email, &item.Plan, &item.UpgradeType, &item.ActivatedAt,
			&item.SubscriptionExpiresAt, &estimated, &item.UpstreamID, &item.CardID, &item.MarketplaceOrderID); err != nil {
			localError(c, 500, "读取客户列表失败")
			return
		}
		item.ExpiryEstimated = estimated == 1
		list = append(list, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		localError(c, 500, "读取客户列表失败")
		return
	}
	rows.Close()
	_ = tx.Rollback()
	orderIDs := make([]string, 0, len(list))
	seenOrders := map[string]bool{}
	for _, item := range list {
		if item.MarketplaceOrderID != "" && !seenOrders[item.MarketplaceOrderID] {
			seenOrders[item.MarketplaceOrderID] = true
			orderIDs = append(orderIDs, item.MarketplaceOrderID)
		}
	}
	identityNotice := ""
	for start := 0; start < len(orderIDs); start += 100 {
		end := start + 100
		if end > len(orderIDs) {
			end = len(orderIDs)
		}
		identities, lookupErr := marketplaceCustomerIdentities(c, orderIDs[start:end])
		if lookupErr != nil {
			identityNotice = "购买人资料暂时无法从商城读取；升级账号与订单数据仍可查看和导出。"
			break
		}
		for i := range list {
			identity, ok := identities[list[i].MarketplaceOrderID]
			if !ok {
				continue
			}
			list[i].BuyerName = identity.BuyerName
			list[i].BuyerEmail = identity.BuyerEmail
			list[i].TelegramID = identity.TelegramID
			list[i].TelegramUsername = identity.TelegramUsername
		}
	}

	c.JSON(200, gin.H{
		"generated_at":    now,
		"timezone":        location.String(),
		"plans":           plans,
		"totals":          totals,
		"list":            list,
		"total":           total,
		"page":            req.Page,
		"page_size":       req.PageSize,
		"expiry_notice":   "上游暂未返回精确订阅截止时间；标记为预计的日期按升级成功时间加一个自然月计算。",
		"identity_notice": identityNotice,
		"data_quality": gin.H{
			"completed":     completed,
			"missing_email": missingEmail,
			"missing_dates": missingDates,
		},
	})
}
