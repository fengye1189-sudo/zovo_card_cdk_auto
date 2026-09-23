package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
	"github.com/tuzi/cdk-recharge-system/internal/gptcheck"
)

func accounthubBaseURL() string {
	if u := strings.TrimSpace(os.Getenv("ACCOUNTHUB_BASE_URL")); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://localhost:8788"
}

// extractEmailFromSession 从 session JSON 提取 user.email。
func extractEmailFromSession(raw string) string {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "{") {
		return ""
	}
	var data map[string]interface{}
	if json.Unmarshal([]byte(s), &data) != nil {
		return ""
	}
	if user, ok := data["user"].(map[string]interface{}); ok {
		if email, ok := user["email"].(string); ok {
			return strings.TrimSpace(email)
		}
	}
	if email, ok := data["email"].(string); ok {
		return strings.TrimSpace(email)
	}
	return ""
}

type acchubInvoiceResp struct {
	InvoiceURL string                   `json:"invoice_url"`
	Invoices   []map[string]interface{} `json:"invoices"`
	Source     string                   `json:"source"`
	Error      string                   `json:"error"`
}

// queryAccounthubInvoices 通过邮箱调 accounthub 获取账单。
func queryAccounthubInvoices(email string) (*acchubInvoiceResp, error) {
	base := accounthubBaseURL()
	u := fmt.Sprintf("%s/gpt/invoices-by-email?email=%s", base, url.QueryEscape(email))
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		// url.Error includes the full URL and therefore the private account email.
		// Return a fixed error instead of propagating that URL into application logs.
		return nil, errors.New("accounthub request unavailable")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("accounthub status %d", resp.StatusCode)
	}
	var out acchubInvoiceResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("accounthub 响应解析失败")
	}
	return &out, nil
}

func invoiceCreatedUnix(v interface{}) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), t > 0
	case int64:
		return t, t > 0
	case json.Number:
		n, err := t.Int64()
		return n, err == nil && n > 0
	case string:
		s := strings.TrimSpace(t)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			return n, true
		}
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts.Unix(), true
		}
	}
	return 0, false
}

// safeCDKInvoice returns at most the invoice closest to this redemption.
// Bearer links are deliberately removed: a CDK grant proves access to this
// redemption for seven days, not to every historical invoice for the email.
func safeCDKInvoice(invoices []map[string]interface{}, submittedAt int64) []map[string]interface{} {
	var best map[string]interface{}
	var bestDistance int64
	for _, inv := range invoices {
		created, ok := invoiceCreatedUnix(inv["created"])
		// The provider endpoint is keyed by account email rather than our order
		// ID. Only accept the invoice produced near this redemption; later
		// purchases on the same account must never be exposed by this CDK.
		if !ok || created < submittedAt-15*60 || created > submittedAt+2*60*60 {
			continue
		}
		distance := created - submittedAt
		if distance < 0 {
			distance = -distance
		}
		if best == nil || distance < bestDistance {
			best = inv
			bestDistance = distance
		}
	}
	if best == nil {
		return []map[string]interface{}{}
	}
	safe := map[string]interface{}{}
	for _, key := range []string{"number", "status", "paid", "currency", "total", "amount_paid", "created", "description"} {
		if value, ok := best[key]; ok && value != nil {
			safe[key] = value
		}
	}
	return []map[string]interface{}{safe}
}

// legacyCDKBillingIdentity gives orders created before public bindings were
// introduced the same fixed seven-day grant. The first task row is the
// immutable identity anchor: a later retry must never replace its customer
// email or move the window forward.
func legacyCDKBillingIdentity(cdk string, now time.Time) (string, int64, int64, bool, error) {
	if db.DB == nil {
		return "", 0, 0, false, sql.ErrConnDone
	}
	var hasUnanchoredTask int
	if err := db.DB.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM recharge_tasks
			WHERE upper(trim(cdk_code)) = upper(trim(?))
			  AND strftime('%s',created_at) IS NULL
		)
	`, cdk).Scan(&hasUnanchoredTask); err != nil {
		return "", 0, 0, false, err
	}
	if hasUnanchoredTask == 1 {
		return "", 0, 0, true, db.ErrCDKBindingMismatch
	}
	var email string
	var taskCreated int64
	err := db.DB.QueryRow(`
		SELECT COALESCE(account_email,''),
		       COALESCE(CAST(strftime('%s',created_at) AS INTEGER),0)
		FROM recharge_tasks
		WHERE upper(trim(cdk_code)) = upper(trim(?))
		  AND strftime('%s',created_at) IS NOT NULL
		ORDER BY CAST(strftime('%s',created_at) AS INTEGER) ASC, rowid ASC
		LIMIT 1
	`, cdk).Scan(&email, &taskCreated)
	if err == sql.ErrNoRows {
		return "", 0, 0, false, nil
	}
	if err != nil {
		return "", 0, 0, false, err
	}
	if taskCreated <= 0 {
		return "", 0, 0, false, nil
	}
	// Legacy tasks predate immutable attempt bindings. If more than one account
	// identity exists for the same CDK, fail closed instead of guessing which
	// customer's invoice belongs to the caller.
	var distinctEmails int
	var uniqueEmail sql.NullString
	if err := db.DB.QueryRow(`
		SELECT COUNT(DISTINCT lower(trim(account_email))),
		       MIN(NULLIF(trim(account_email),''))
		FROM recharge_tasks
		WHERE upper(trim(cdk_code)) = upper(trim(?))
		  AND trim(COALESCE(account_email,'')) <> ''
	`, cdk).Scan(&distinctEmails, &uniqueEmail); err != nil {
		return "", 0, 0, false, err
	}
	if distinctEmails > 1 {
		return "", 0, 0, true, db.ErrCDKBindingMismatch
	}
	if distinctEmails == 1 && uniqueEmail.Valid {
		email = strings.TrimSpace(uniqueEmail.String)
	}

	submittedAt := taskCreated
	var usedAt int64
	err = db.DB.QueryRow(`
		SELECT COALESCE(CAST(strftime('%s',used_at) AS INTEGER),0)
		FROM cd_keys
		WHERE upper(trim(code)) = upper(trim(?))
		LIMIT 1
	`, cdk).Scan(&usedAt)
	if err != nil && err != sql.ErrNoRows {
		return "", 0, 0, false, err
	}
	if usedAt > 0 && usedAt < submittedAt {
		submittedAt = usedAt
	}
	queryExpiresAt := submittedAt + int64(publicCDKQueryWindow/time.Second)
	if now.UTC().Unix() >= queryExpiresAt {
		return "", submittedAt, queryExpiresAt, true, db.ErrCDKQueryExpired
	}
	return strings.TrimSpace(email), submittedAt, queryExpiresAt, true, nil
}

func billingByCDK(c *gin.Context, cdk string) {
	now := time.Now()
	var email string
	var submittedAt, queryExpiresAt int64
	var localAuthorization *localPublicCDK
	var bindingAuthorization *db.CDKBinding
	legacyAuthorization := false

	local, lerr := loadLocalPublicCDK(cdk, now)
	if local != nil {
		if errors.Is(lerr, errLocalCDKQueryExpired) {
			c.JSON(http.StatusGone, gin.H{"error": "7 天查询期已结束；如需售后，请联系客服并提供原始卡密。", "status": "query_expired"})
			return
		}
		if lerr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "查询暂时不可用"})
			return
		}
		if local.SubmittedAt <= 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "该卡密尚未提交兑换，暂无账单记录。"})
			return
		}
		email = strings.TrimSpace(local.Email)
		submittedAt = local.SubmittedAt
		queryExpiresAt = local.QueryExpiresAt
		localAuthorization = local
	} else if lerr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "查询暂时不可用"})
		return
	} else {
		binding, err := db.GetPublicCDKBindingByCode(cdk, now)
		if errors.Is(err, db.ErrCDKQueryExpired) {
			c.JSON(http.StatusGone, gin.H{"error": "7 天查询期已结束；如需售后，请联系客服并提供原始卡密。", "status": "query_expired"})
			return
		}
		if errors.Is(err, db.ErrCDKQueryNotStarted) {
			// A fresh successful preview starts a new pending attempt and must
			// shadow every older task/invoice for the same reusable CDK.
			c.JSON(http.StatusNotFound, gin.H{"error": "该卡密尚未提交本次兑换，暂无账单记录。", "status": "not_submitted"})
			return
		}
		if err != nil && !errors.Is(err, db.ErrCDKQueryNotStarted) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "查询绑定失败"})
			return
		}
		if binding == nil {
			var found bool
			email, submittedAt, queryExpiresAt, found, err = legacyCDKBillingIdentity(cdk, now)
			if errors.Is(err, db.ErrCDKQueryExpired) {
				c.JSON(http.StatusGone, gin.H{"error": "7 天查询期已结束；如需售后，请联系客服并提供原始卡密。", "status": "query_expired"})
				return
			}
			if errors.Is(err, db.ErrCDKBindingMismatch) {
				c.JSON(http.StatusNotFound, gin.H{"error": "无法安全确认该历史兑换记录的账单归属，请联系客服核对。"})
				return
			}
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "查询暂时不可用"})
				return
			}
			if !found {
				c.JSON(http.StatusNotFound, gin.H{"error": "未找到该卡密已提交的兑换记录。"})
				return
			}
			legacyAuthorization = true
		} else {
			bindingAuthorization = binding
			email = strings.TrimSpace(binding.AccountEmail)
			if email == "" {
				// Compatibility for old, still-active rows only. The session is never
				// sent to another fallback checker and is scrubbed at expiry.
				email = extractEmailFromSession(binding.SessionPayload)
			}
			if email == "" && strings.TrimSpace(binding.RedemptionToken) != "" {
				// Redeem responses do not always contain the account email. Resolve it
				// from the immutable current-attempt token so a customer who closed the
				// page immediately after submit can still query this attempt's bill.
				cli := cardplatform.NewFromSettings()
				st, raw, resultErr := cli.Result(c.Request.Context(), binding.RedemptionToken, deviceFrom(c))
				if resultErr != nil || st < 200 || st >= 300 {
					// Network errors may embed the request URL, including the private
					// redemption token. Never write the raw error to logs.
					log.Printf("[billing] result unavailable tok=%s status=%d network_error=%t", shortTok(binding.RedemptionToken), st, resultErr != nil)
					c.JSON(http.StatusBadGateway, gin.H{"error": "兑换结果暂时不可用，无法安全确认账单归属，请稍后重试。"})
					return
				}
				var payload map[string]any
				if json.Unmarshal(raw, &payload) != nil || payload == nil {
					c.JSON(http.StatusBadGateway, gin.H{"error": "兑换结果暂时不可用，无法安全确认账单归属，请稍后重试。"})
					return
				}
				resolved := strings.TrimSpace(extractPublicResultEmail(payload))
				if resolved != "" {
					if bindErr := db.BindCDKAccountEmailForAttempt(binding.CDKCode, binding.RedemptionToken, binding.AttemptNonce, resolved); bindErr != nil {
						log.Printf("[billing] account binding changed tok=%s: %v", shortTok(binding.RedemptionToken), bindErr)
						c.JSON(http.StatusConflict, gin.H{"error": "兑换记录状态已变化，请重新使用卡密查询。"})
						return
					}
					email = resolved
				}
			}
			submittedAt = binding.QueryStartedAt
			queryExpiresAt = binding.QueryExpiresAt
		}
	}

	if email == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "该兑换记录暂未生成可查询账单，请稍后再试。"})
		return
	}
	inv, err := queryAccounthubInvoices(email)
	if err != nil {
		log.Printf("[billing] accounthub unavailable for %s", maskEmail(email))
		c.JSON(http.StatusBadGateway, gin.H{"error": "账单服务暂时不可用，请稍后重试。"})
		return
	}

	// The external invoice lookup is a network round trip. Revalidate the exact
	// authorization after it returns so an attempt rotation, a newly discovered
	// earlier submission anchor, or the exact seven-day boundary cannot expose a
	// stale customer's invoice.
	finalNow := time.Now()
	if queryExpiresAt <= 0 || finalNow.UTC().Unix() >= queryExpiresAt {
		c.JSON(http.StatusGone, gin.H{"error": "7 天查询期已结束；如需售后，请联系客服并提供原始卡密。", "status": "query_expired"})
		return
	}
	switch {
	case bindingAuthorization != nil:
		current, currentErr := db.GetPublicCDKBindingByCode(cdk, finalNow)
		if errors.Is(currentErr, db.ErrCDKQueryExpired) {
			c.JSON(http.StatusGone, gin.H{"error": "7 天查询期已结束；如需售后，请联系客服并提供原始卡密。", "status": "query_expired"})
			return
		}
		if currentErr != nil && !errors.Is(currentErr, db.ErrCDKQueryNotStarted) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "查询暂时不可用"})
			return
		}
		if current == nil || currentErr != nil ||
			current.CDKCode != bindingAuthorization.CDKCode ||
			current.RedemptionToken != bindingAuthorization.RedemptionToken ||
			current.AttemptNonce != bindingAuthorization.AttemptNonce ||
			current.QueryStartedAt != submittedAt || current.QueryExpiresAt != queryExpiresAt {
			c.JSON(http.StatusConflict, gin.H{"error": "兑换记录状态已变化，请重新使用原始卡密查询。"})
			return
		}
	case localAuthorization != nil:
		current, currentErr := loadLocalPublicCDK(cdk, finalNow)
		if errors.Is(currentErr, errLocalCDKQueryExpired) {
			c.JSON(http.StatusGone, gin.H{"error": "7 天查询期已结束；如需售后，请联系客服并提供原始卡密。", "status": "query_expired"})
			return
		}
		if currentErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "查询暂时不可用"})
			return
		}
		if current == nil || current.ID != localAuthorization.ID ||
			current.SubmittedAt != submittedAt || current.QueryExpiresAt != queryExpiresAt ||
			!strings.EqualFold(strings.TrimSpace(current.Email), email) {
			c.JSON(http.StatusConflict, gin.H{"error": "兑换记录状态已变化，请重新使用原始卡密查询。"})
			return
		}
	case legacyAuthorization:
		currentEmail, currentSubmittedAt, currentExpiresAt, found, currentErr := legacyCDKBillingIdentity(cdk, finalNow)
		if errors.Is(currentErr, db.ErrCDKQueryExpired) {
			c.JSON(http.StatusGone, gin.H{"error": "7 天查询期已结束；如需售后，请联系客服并提供原始卡密。", "status": "query_expired"})
			return
		}
		if currentErr != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "无法安全确认该历史兑换记录的账单归属，请联系客服核对。"})
			return
		}
		if !found || currentSubmittedAt != submittedAt || currentExpiresAt != queryExpiresAt ||
			!strings.EqualFold(strings.TrimSpace(currentEmail), email) {
			c.JSON(http.StatusConflict, gin.H{"error": "兑换记录状态已变化，请重新使用原始卡密查询。"})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"summary": gin.H{
			"email":             maskEmail(email),
			"submitted_at":      time.Unix(submittedAt, 0).UTC().Format(time.RFC3339),
			"query_expires_at":  time.Unix(queryExpiresAt, 0).UTC().Format(time.RFC3339),
			"query_window_days": 7,
			"billing_scope":     "this_redemption",
		},
		"invoices":         safeCDKInvoice(inv.Invoices, submittedAt),
		"auth_source":      "cdk",
		"billing_provider": "accounthub",
	})
}

// SessionBillingCheck POST /api/v1/public/billing/check
// Supports a seven-day CDK grant or an explicitly supplied customer session.
func SessionBillingCheck(c *gin.Context) {
	privateNoStore(c)
	var req struct {
		TokenInput string `json:"token_input"`
		Session    string `json:"session"`
		CDKCode    string `json:"cdk_code"`
		Code       string `json:"code"`
	}
	if !localBody(c, &req) {
		return
	}

	raw := strings.TrimSpace(req.TokenInput)
	if raw == "" {
		raw = strings.TrimSpace(req.Session)
	}
	cdk := strings.TrimSpace(req.CDKCode)
	if cdk == "" {
		cdk = strings.TrimSpace(req.Code)
	}

	if raw == "" && cdk != "" {
		cdk = normalizePublicCDK(cdk)
		if !validPublicCDK(cdk) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请输入完整卡密"})
			return
		}
		billingByCDK(c, cdk)
		return
	}

	if raw == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请输入卡密，或粘贴 session JSON / accessToken"})
		return
	}

	// Explicit session mode remains available for the account owner and is not
	// coupled to a CDK public-query grant.
	res, err := gptcheck.Check(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"summary":     res.Summary,
		// Issuer hosted/PDF links can behave as bearer URLs. A public session
		// diagnostic may return invoice metadata, but never those links.
		"invoices":    publicInvoiceMetadata(res.Invoices),
		"auth_source": "session",
	})
}
