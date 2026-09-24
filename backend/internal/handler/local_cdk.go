package handler

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

const mapleSupportURL = "https://t.me/fengye1189"

func localHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func localRandom() (string, error) {
	b := make([]byte, 24)
	_, e := rand.Read(b)
	return hex.EncodeToString(b), e
}
func localError(c *gin.Context, status int, s string) { c.JSON(status, gin.H{"error": s}) }
func localBody(c *gin.Context, v any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 65536)
	if c.ShouldBindJSON(v) != nil {
		localError(c, 400, "请求格式不正确")
		return false
	}
	return true
}

// Bounded, per-process public request limiter. Tokens/codes are sent in POST
// bodies (never URL query strings, which Gin logs).
func publicRequestLimit(maxPerMinute int) gin.HandlerFunc {
	type window struct {
		n     int
		until time.Time
	}
	var mu sync.Mutex
	entries := map[string]window{}
	return func(c *gin.Context) {
		privateNoStore(c)
		ip := c.ClientIP()
		now := time.Now()
		mu.Lock()
		if len(entries) > 10000 {
			for k, v := range entries {
				if now.After(v.until) {
					delete(entries, k)
				}
			}
		}
		v, exists := entries[ip]
		if !exists && len(entries) >= 20000 {
			mu.Unlock()
			c.AbortWithStatus(429)
			return
		}
		if now.After(v.until) {
			v = window{until: now.Add(time.Minute)}
		}
		v.n++
		entries[ip] = v
		mu.Unlock()
		if v.n > maxPerMinute {
			c.AbortWithStatusJSON(429, gin.H{"error": "操作频繁，请稍后再试"})
			return
		}
		c.Next()
	}
}

func LocalCDKLimit() gin.HandlerFunc { return publicRequestLimit(60) }

// PublicCDKMutationLimit leaves enough headroom for two legitimate 100-item
// batch runs while bounding upstream preview/preflight/redeem amplification.
// Read-only token polling remains outside this limiter.
func PublicCDKMutationLimit() gin.HandlerFunc { return publicRequestLimit(240) }

type localSettings struct {
	ProDedicatedEnabled          bool    `json:"pro_dedicated_enabled"`
	UseUpstreamCardLimit         bool    `json:"use_upstream_card_limit"`
	Enabled                      bool    `json:"enabled"`
	CardID                       int64   `json:"card_id"`
	CardIDs                      []int64 `json:"card_ids"`
	MinCardBalanceMinor          int64   `json:"min_card_balance_minor"`
	Currency                     string  `json:"currency"`
	MaxAmountMinor               int64   `json:"max_amount_minor"`
	MaxFeeMinor                  int64   `json:"max_fee_minor"`
	MaxSuccessfulPaymentsPerCard int     `json:"max_successful_payments_per_card"`
}

func localCardIDs(s localSettings) []int64 {
	if s.CardIDs != nil {
		return s.CardIDs
	}
	if s.CardID > 0 {
		return []int64{s.CardID}
	}
	return []int64{}
}

// Retained for settings compatibility with older browser drafts. The current
// policy has no local success-count cap.
func localPaymentLimit(s localSettings) int {
	return 0
}
func localCardHasPaymentCapacity(id int64, limit int) (bool, error) {
	kind, allowed, err := localCardKind(id, true)
	if err != nil || !allowed {
		return false, err
	}
	return localCardHasCycleCapacity(id, kind, time.Now().Unix())
}
func localShuffleCards(ids []int64) error {
	for i := len(ids) - 1; i > 0; i-- {
		n, e := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if e != nil {
			return e
		}
		j := int(n.Int64())
		ids[i], ids[j] = ids[j], ids[i]
	}
	return nil
}

// localFairShuffleCards keeps randomness among equally used cards while
// preventing consecutive use of the same card when another eligible card is
// available. Persisted selection counts make the distribution survive restarts.
func localFairShuffleCards(ids []int64) error {
	if err := localShuffleCards(ids); err != nil || len(ids) < 2 {
		return err
	}
	allowed := make(map[int64]bool, len(ids))
	for _, id := range ids {
		allowed[id] = true
	}
	counts := map[int64]int64{}
	rows, err := db.DB.Query("SELECT card_id,COUNT(*) FROM local_card_selections GROUP BY card_id")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, count int64
		if err = rows.Scan(&id, &count); err != nil {
			rows.Close()
			return err
		}
		counts[id] = count
	}
	if err = rows.Close(); err != nil {
		return err
	}
	lastCard := int64(0)
	rows, err = db.DB.Query("SELECT card_id FROM local_card_selections ORDER BY selected_at DESC,local_id DESC")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if allowed[id] {
			lastCard = id
			break
		}
	}
	if err = rows.Close(); err != nil {
		return err
	}
	sort.SliceStable(ids, func(i, j int) bool {
		iLast, jLast := ids[i] == lastCard, ids[j] == lastCard
		if iLast != jLast {
			return !iLast
		}
		return counts[ids[i]] < counts[ids[j]]
	})
	return nil
}
func localAvailableCards(c *gin.Context, cli *cardplatform.Client, s localSettings, plans ...string) ([]int64, error) {
	plan := "plus"
	if len(plans) > 0 {
		plan = plans[0]
	}
	candidates, e := cli.DirectCandidatesForPlan(c.Request.Context(), plan)
	if e != nil {
		return nil, e
	}
	usable := map[int64]bool{}
	var existingInventory []cardplatform.CardChoice
	for _, v := range candidates {
		if v.CardID == 290694 && v.SkipReason == "渠道 星链卡 不可用于自动开卡" {
			// Failed verification leaves the original exclusion intact.
			existingInventory, _ = automationInventory(c.Request.Context(), cli)
			break
		}
	}
	for _, v := range candidates {
		v = existingPaymentCandidate(v, existingInventory, plan)
		usable[v.CardID] = v.Usable(s.MinCardBalanceMinor)
	}
	ids, kinds, e := localPoolCards(s, time.Now().Unix())
	if e != nil {
		return nil, e
	}
	available := make([]int64, 0, len(ids))
	for _, id := range ids {
		if isProDedicatedPlan(plan) && kinds[id] == "pro" {
			continue
		}
		if !usable[id] {
			continue
		}
		var busy int
		if e = db.DB.QueryRow("SELECT COUNT(*) FROM local_cdks WHERE card_id=? AND status IN ('reserved','review')", id).Scan(&busy); e != nil {
			return nil, e
		}
		if busy == 0 {
			available = append(available, id)
		}
	}
	if e := localFairShuffleCards(available); e != nil {
		return nil, e
	}
	return available, nil
}
func readLocalSettings() localSettings {
	var s localSettings
	raw, _ := db.GetSetting("local_cdk_settings")
	_ = json.Unmarshal([]byte(raw), &s)
	return s
}
func LocalCDKGetSettings(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	raw, e := db.GetSetting("local_cdk_settings")
	if e != nil {
		localError(c, 500, "读取设置失败，请重试")
		return
	}
	var s localSettings
	if raw != "" && json.Unmarshal([]byte(raw), &s) != nil {
		localError(c, 500, "服务器设置格式异常，请联系管理员恢复备份")
		return
	}
	var savedAt string
	_ = db.DB.QueryRow("SELECT COALESCE(updated_at,'') FROM site_settings WHERE key='local_cdk_settings'").Scan(&savedAt)
	c.JSON(200, gin.H{"settings": s, "revision": localHash(raw), "saved_at": savedAt, "api_configured": cardplatform.LoadConfig().APIKey != ""})
}
func LocalCDKPutSettings(c *gin.Context) {
	var req struct {
		localSettings
		ExpectedRevision string `json:"expected_revision"`
	}
	if !localBody(c, &req) {
		return
	}
	s := req.localSettings
	s.ProDedicatedEnabled = readLocalSettings().ProDedicatedEnabled
	s.UseUpstreamCardLimit = true // Old browser drafts must not restore the retired local cap.
	s.Currency = strings.ToUpper(strings.TrimSpace(s.Currency))
	if len(req.ExpectedRevision) != 64 {
		localError(c, 409, "页面版本已过期，请刷新设置后重新保存")
		return
	}
	if s.MaxSuccessfulPaymentsPerCard == 0 {
		s.MaxSuccessfulPaymentsPerCard = 3
	}
	ids := localCardIDs(s)
	seen := map[int64]bool{}
	if len(ids) > 20 || s.MinCardBalanceMinor < 0 {
		localError(c, 400, "最多指定 20 张卡，余额门槛不能为负数")
		return
	}
	if s.MaxSuccessfulPaymentsPerCard < 1 || s.MaxSuccessfulPaymentsPerCard > 100 {
		localError(c, 400, "每张卡最多成功支付次数应为 1–100")
		return
	}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			localError(c, 400, "卡片 ID 必须为正整数且不能重复")
			return
		}
		seen[id] = true
	}
	s.CardIDs = ids
	s.CardID = 0
	if s.Enabled && (len(ids) == 0 || s.MinCardBalanceMinor <= 0 || len(s.Currency) != 3 || s.MaxAmountMinor <= 0 || s.MaxFeeMinor < 0 || cardplatform.LoadConfig().APIKey == "") {
		localError(c, 400, "开启前请配置 API 密钥、支付卡白名单、正数余额门槛、币种及费用上限")
		return
	}
	raw, _ := json.Marshal(s)
	tx, e := db.DB.Begin()
	if e != nil {
		localError(c, 503, "保存繁忙，请重试")
		return
	}
	defer tx.Rollback()
	var previous string
	e = tx.QueryRow("SELECT value FROM site_settings WHERE key='local_cdk_settings'").Scan(&previous)
	if e != nil && e != sql.ErrNoRows {
		localError(c, 500, "读取设置失败")
		return
	}
	if req.ExpectedRevision != localHash(previous) {
		localError(c, 409, "服务器设置已更新，当前草稿未覆盖服务器。请放弃旧草稿并重新加载后修改。")
		return
	}
	if _, e = tx.Exec("INSERT INTO site_settings(key,value,updated_at) VALUES('local_cdk_settings',?,CURRENT_TIMESTAMP) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=CURRENT_TIMESTAMP", string(raw)); e != nil {
		localError(c, 503, "保存繁忙，请重新核对后重试")
		return
	}
	if tx.Commit() != nil {
		localError(c, 503, "保存结果待确认，请刷新核对")
		return
	}
	db.WriteAudit(c.GetString("username"), "local_cdk_settings", "updated", c.ClientIP())
	c.JSON(200, gin.H{"ok": true, "revision": localHash(string(raw)), "saved_at": time.Now().UTC().Format(time.RFC3339)})
}
func LocalCDKIssue(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var req struct {
		Count     int    `json:"count"`
		Days      int    `json:"days"`
		RequestID string `json:"request_id"`
		ProductID string `json:"product_id"`
	}
	if !localBody(c, &req) {
		return
	}
	product, e := operationsProductForIssue(req.ProductID)
	if e != nil {
		localError(c, 409, "商品不存在或已停用，请刷新商品列表")
		return
	}
	req.Days = 90
	if req.Count < 1 || req.Count > 100 || req.Days < 1 || req.Days > 365 || len(req.RequestID) < 16 || len(req.RequestID) > 80 {
		localError(c, 400, "数量为 1–100，有效期为 1–365 天，请提供批次编号")
		return
	}
	tx, e := db.DB.Begin()
	if e != nil {
		localError(c, 503, "数据库繁忙")
		return
	}
	defer tx.Rollback()
	var productEnabled bool
	if e = tx.QueryRow("SELECT enabled FROM operations_products WHERE id=? AND plan=?", product.ID, product.Plan).Scan(&productEnabled); e != nil || !productEnabled {
		localError(c, 409, "商品已停用，未生成卡密")
		return
	}
	var n int
	if e = tx.QueryRow("SELECT COUNT(*) FROM local_cdks WHERE batch_id=?", req.RequestID).Scan(&n); e != nil {
		localError(c, 500, "查询批次失败")
		return
	}
	if n > 0 {
		localError(c, 409, "该批次已经生成。完整码仅返回一次，请勿重复发码；若未保存，请禁用该批次后重新生成。")
		return
	}
	codes := []string{}
	now := time.Now().Unix()
	for i := 0; i < req.Count; i++ {
		code, err := GenerateCDKCode(product.Plan)
		if err != nil {
			localError(c, 500, "生成失败")
			return
		}
		label := localCodeLabel(product.Plan)
		code = label + code
		result, err := tx.Exec("INSERT INTO local_cdks(code_hash,prefix,plan,expires_at,created_at,batch_id) VALUES(?,?,?,?,?,?)", localHash(code), code[:len(label)+4], product.Plan, now+int64(req.Days)*86400, now, req.RequestID)
		if err != nil {
			localError(c, 503, "发码失败，请用同一批次编号重试")
			return
		}
		id, err := result.LastInsertId()
		if err != nil || operationsBindIssuedProduct(tx, id, product.ID) != nil {
			localError(c, 503, "商品绑定失败，未生成卡密")
			return
		}
		codes = append(codes, code)
	}
	if tx.Commit() != nil {
		localError(c, 503, "发码结果待确认，请刷新列表核对批次")
		return
	}
	db.WriteAudit(c.GetString("username"), "local_cdk_issue", fmt.Sprintf("batch=%s count=%d", req.RequestID, req.Count), c.ClientIP())
	c.JSON(200, gin.H{"codes": codes, "batch_id": req.RequestID, "plan": product.Plan, "product_id": product.ID, "expires_at": now + int64(req.Days)*86400})
}
func LocalCDKList(c *gin.Context) {
	OperationsRecordsList(c)
}
func LocalCDKDisable(c *gin.Context) {
	r, e := db.DB.Exec("UPDATE local_cdks SET status='disabled',token_hash='',preflight_hash='' WHERE id=? AND status='unused' AND request_id='' AND upstream_id=0", c.Param("id"))
	if e != nil {
		localError(c, 500, "禁用失败")
		return
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		localError(c, 409, "仅可禁用未兑换卡密")
		return
	}
	db.WriteAudit(c.GetString("username"), "local_cdk_disable", c.Param("id"), c.ClientIP())
	c.JSON(200, gin.H{"ok": true})
}

type localCode struct {
	ID, Expires, TokenExpires, PFExpires, CardID, Version, Upstream, LastChecked int64
	ActivatedAt, SubscriptionExpiresAt                                           int64
	Plan, Status, Token, Device, PF, Cred, Request, Email, Message, UpgradeType  string
	ExpiryEstimated                                                              bool
}

func loadLocal(where, value string) (localCode, error) {
	var r localCode
	var expiryEstimated int
	e := db.DB.QueryRow("SELECT id,plan,status,expires_at,token_hash,device_hash,token_expires,preflight_hash,credential_hash,preflight_expires,card_id,pricing_version,request_id,upstream_id,email,message,last_checked,activated_at,subscription_expires_at,upgrade_type,expiry_estimated FROM local_cdks WHERE "+where+"=?", value).Scan(&r.ID, &r.Plan, &r.Status, &r.Expires, &r.Token, &r.Device, &r.TokenExpires, &r.PF, &r.Cred, &r.PFExpires, &r.CardID, &r.Version, &r.Request, &r.Upstream, &r.Email, &r.Message, &r.LastChecked, &r.ActivatedAt, &r.SubscriptionExpiresAt, &r.UpgradeType, &expiryEstimated)
	r.ExpiryEstimated = expiryEstimated == 1
	return r, e
}
func localDevice(c *gin.Context) string { return localHash(c.GetHeader("X-Redemption-Device")) }
func localResultWithinWindow(c *gin.Context, r localCode) bool {
	if r.Status == "unused" {
		return true
	}
	submittedAt, expiresAt, err := localQueryWindowByID(r.ID, time.Now())
	if err != nil && !errors.Is(err, errLocalCDKQueryExpired) {
		localError(c, http.StatusServiceUnavailable, "暂时无法核对查询期限，请稍后重试")
		return false
	}
	if errors.Is(err, errLocalCDKQueryExpired) || submittedAt <= 0 {
		c.JSON(http.StatusGone, gin.H{
			"error":            "7 天查询期已结束；如需售后，请联系客服。",
			"status":           "query_expired",
			"query_expires_at": expiresAt,
		})
		return false
	}
	return true
}
func localToken(c *gin.Context, token string) (localCode, bool) {
	r, e := loadLocal("token_hash", localHash(token))
	if token == "" || e != nil || c.GetHeader("X-Redemption-Device") == "" || r.Device != localDevice(c) || r.TokenExpires < time.Now().Unix() {
		localError(c, 401, "兑换会话已失效，请重新输入卡密")
		return r, false
	}
	if !localResultWithinWindow(c, r) {
		return r, false
	}
	return r, true
}
func LocalCDKPreview(c *gin.Context) {
	var req struct {
		Code string `json:"code"`
	}
	if !localBody(c, &req) {
		return
	}
	if c.GetHeader("X-Redemption-Device") == "" {
		localError(c, 400, "缺少设备标识")
		return
	}
	r, e := loadLocal("code_hash", localHash(strings.ToUpper(strings.TrimSpace(req.Code))))
	if e != nil || r.Status == "disabled" || (r.Status == "unused" && r.Expires <= time.Now().Unix()) {
		localError(c, 400, "卡密无效、已禁用或已过期")
		return
	}
	if !localResultWithinWindow(c, r) {
		return
	}
	if r.Status == "unused" && !operationsProductAvailable(r.ID) {
		localError(c, 409, "该卡密对应商品已暂停兑换，请联系商家")
		return
	}
	tok, e := localRandom()
	if e != nil {
		localError(c, 500, "验证失败")
		return
	}
	_, e = db.DB.Exec("UPDATE local_cdks SET token_hash=?,device_hash=?,token_expires=?,preflight_hash='',credential_hash='' WHERE id=?", localHash(tok), localDevice(c), time.Now().Add(time.Hour).Unix(), r.ID)
	if e != nil {
		localError(c, 503, "验证失败，请重试")
		return
	}
	c.JSON(200, gin.H{"redemption_token": tok, "plan": r.Plan, "status": r.Status, "message": "本站卡密验证成功"})
}
func credentialBinding(credential json.RawMessage, s localSettings) string {
	var v any
	if json.Unmarshal(credential, &v) != nil {
		return ""
	}
	b, _ := json.Marshal(v)
	cfg, _ := json.Marshal(s)
	return localHash(string(b) + string(cfg))
}
func LocalCDKPreflight(c *gin.Context) {
	var req struct {
		Token      string          `json:"redemption_token"`
		Credential json.RawMessage `json:"credential"`
	}
	if !localBody(c, &req) {
		return
	}
	r, ok := localToken(c, req.Token)
	if !ok {
		return
	}
	if r.Status != "unused" || r.Expires <= time.Now().Unix() {
		localError(c, 409, "卡密正在处理或已使用，请查询结果")
		return
	}
	if !operationsProductAvailable(r.ID) {
		localError(c, 409, "该商品已暂停兑换，请联系商家")
		return
	}
	s := settingsForLocalPlan(readLocalSettings(), r.Plan)
	if !s.Enabled || (r.Plan == "pro_5x" && !s.ProDedicatedEnabled) || (!(isProDedicatedPlan(r.Plan) && s.ProDedicatedEnabled) && (len(localCardIDs(s)) == 0 || s.MinCardBalanceMinor <= 0)) {
		localError(c, 503, "卡密有效，充值通道尚未开放，请联系商家")
		return
	}
	var cred struct {
		Mode    string `json:"mode"`
		Session string `json:"session"`
	}
	if json.Unmarshal(req.Credential, &cred) != nil || cred.Mode != "session" || len(cred.Session) < 40 {
		localError(c, 400, "请提供有效的账号 Session，本通道不收集邮箱密码")
		return
	}
	cli := cardplatform.NewFromSettings()
	version, fee, e := cli.DirectPricing(c.Request.Context(), r.Plan)
	if e != nil {
		localError(c, 502, "暂时无法获取充值报价，请联系商家检查通道")
		return
	}
	if fee > s.MaxFeeMinor {
		localError(c, 503, "套餐未开放或服务费超出商家设置的限额")
		return
	}
	raw, e := cli.DirectPreflight(c.Request.Context(), gin.H{"product": "gpt", "credential": req.Credential})
	if e != nil {
		localError(c, 502, "账号预检失败，请检查账号信息或联系商家")
		return
	}
	var pf struct {
		Token      string `json:"preflight_token"`
		Expires    string `json:"preflight_expires_at"`
		Email      string `json:"email"`
		Plan       string `json:"currentPlan"`
		QuoteError string `json:"quote_error"`
		Quotes     map[string]struct {
			Amount   int64  `json:"amountMinor"`
			Currency string `json:"currency"`
		} `json:"quotes"`
	}
	if json.Unmarshal(raw, &pf) != nil || pf.Token == "" || pf.Email == "" || pf.QuoteError != "" {
		localError(c, 502, "账号或报价信息不完整，尚未提交充值")
		return
	}
	quote := pf.Quotes[r.Plan]
	if quote.Amount <= 0 || quote.Amount > s.MaxAmountMinor || strings.ToUpper(quote.Currency) != s.Currency {
		localError(c, 409, "充值报价超出商家限额或币种不符，尚未扣款")
		return
	}
	expires, e := time.Parse(time.RFC3339, pf.Expires)
	if e != nil || !expires.After(time.Now()) {
		localError(c, 502, "预检有效期不正确，请稍后重试")
		return
	}
	if expires.After(time.Now().Add(10 * time.Minute)) {
		expires = time.Now().Add(10 * time.Minute)
	}
	cards := []int64{0}
	if !isProDedicatedPlan(r.Plan) || !s.ProDedicatedEnabled {
		cards, e = localAvailableCards(c, cli, s, r.Plan)
	}
	if e != nil {
		localError(c, 502, "暂时无法核查支付卡，尚未提交充值")
		return
	}
	if len(cards) == 0 {
		localError(c, 409, "指定卡片均不可用、余额未达门槛或有待核对订单，请联系商家")
		return
	}
	result, e := db.DB.Exec("UPDATE local_cdks SET preflight_hash=?,credential_hash=?,preflight_expires=?,card_id=?,pricing_version=?,email=? WHERE id=? AND status='unused' AND token_hash=?", localHash(pf.Token), credentialBinding(req.Credential, s), expires.Unix(), cards[0], version, pf.Email, r.ID, localHash(req.Token))
	if e != nil {
		localError(c, 503, "保存预检失败")
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		localError(c, 409, "卡密状态已变化，请重新验证")
		return
	}
	// Whitelist public fields; do not forward payment details or upstream secrets.
	c.JSON(200, gin.H{"preflight_token": pf.Token, "email": pf.Email, "current_plan": pf.Plan, "plan": r.Plan, "expires_at": expires.Unix(), "dedicated_card": isProDedicatedPlan(r.Plan) && s.ProDedicatedEnabled})
}
func LocalCDKRedeem(c *gin.Context) {
	var req struct {
		Token      string          `json:"redemption_token"`
		PF         string          `json:"preflight_token"`
		Credential json.RawMessage `json:"credential"`
		Confirm    bool            `json:"confirmed"`
	}
	if !localBody(c, &req) {
		return
	}
	r, ok := localToken(c, req.Token)
	if !ok {
		return
	}
	if r.Status != "unused" {
		c.JSON(202, localPublicResult(r))
		return
	}
	if !operationsProductAvailable(r.ID) {
		localError(c, 409, "该商品已暂停兑换，尚未提交付款")
		return
	}
	s := settingsForLocalPlan(readLocalSettings(), r.Plan)
	if !s.Enabled || !req.Confirm {
		localError(c, 409, "充值未开放或尚未确认")
		return
	}
	autoPolicy, autoVersion, autoError := readAutomationPolicy()
	if autoError != nil || autoPolicy.Paused || automationBlocked() {
		localError(c, 409, "尚未提交充值：系统正在核对资金操作，请稍后重新验证；本次不会扣款，已有订单会继续查询")
		return
	}
	if req.PF == "" || r.PF != localHash(req.PF) || r.Cred != credentialBinding(req.Credential, s) || r.PFExpires <= time.Now().Unix() || r.Expires <= time.Now().Unix() {
		localError(c, 409, "预检已失效或配置发生变化，请重新验证账号")
		return
	}
	cli := cardplatform.NewFromSettings()
	version, fee, e := cli.DirectPricing(c.Request.Context(), r.Plan)
	if e != nil || version != r.Version || fee > s.MaxFeeMinor {
		localError(c, 409, "套餐或报价发生变化，请重新预检；尚未提交充值")
		return
	}
	if isProDedicatedPlan(r.Plan) && s.ProDedicatedEnabled {
		startProDedicated(c, r, req.Token, req.PF, req.Credential, s, autoPolicy, autoVersion, fee)
		return
	}
	cards, e := localAvailableCards(c, cli, s, r.Plan)
	if e != nil {
		localError(c, 502, "暂时无法核查支付卡，尚未提交充值")
		return
	}
	if r.Cred != credentialBinding(req.Credential, settingsForLocalPlan(readLocalSettings(), r.Plan)) {
		localError(c, 409, "商家设置已变化，请重新预检")
		return
	}
	requestID, e := localRandom()
	if e != nil {
		localError(c, 500, "提交失败")
		return
	}
	requestID = "maple-" + requestID
	// Persist the reservation BEFORE calling any payment API. This compare-and-swap
	// is the sole transition that authorizes a call, including across processes.
	chosen := int64(0)
	tx, e := db.DB.Begin()
	if e != nil {
		localError(c, 503, "暂时无法锁定支付卡，尚未提交充值")
		return
	}
	defer tx.Rollback()
	selectedAt := time.Now().Unix()
	for _, id := range cards {
		result, err := tx.Exec("UPDATE local_cdks SET status='reserved',card_id=?,request_id=?,message='订单处理中，请勿重复提交' WHERE id=? AND status='unused' AND token_hash=? AND preflight_hash=? AND credential_hash=? AND preflight_expires>? AND expires_at>? AND NOT EXISTS (SELECT 1 FROM local_cdks WHERE card_id=? AND status IN ('reserved','review')) AND EXISTS(SELECT 1 FROM local_card_cycles WHERE card_id=?) AND EXISTS(SELECT 1 FROM automation_card_lifecycle WHERE card_id=? AND retire_state='active') AND EXISTS(SELECT 1 FROM automation_policy WHERE id=1 AND version=?) AND EXISTS(SELECT 1 FROM operations_products WHERE id=COALESCE((SELECT product_id FROM operations_product_bindings WHERE local_id=?),'plus') AND enabled=1 AND plan=local_cdks.plan) AND NOT EXISTS(SELECT 1 FROM automation_money WHERE state IN ('inflight','unknown','pending'))", id, requestID, r.ID, localHash(req.Token), localHash(req.PF), r.Cred, selectedAt, selectedAt, id, id, id, autoVersion, r.ID)
		if err != nil {
			localError(c, 503, "暂时无法锁定支付卡，尚未提交充值")
			return
		}
		n, err := result.RowsAffected()
		if err != nil {
			localError(c, 503, "暂时无法确认锁定状态，请查询结果")
			return
		}
		if n == 1 {
			if _, err = tx.Exec("INSERT INTO local_card_selections(local_id,card_id,selected_at) VALUES(?,?,?)", r.ID, id, selectedAt); err != nil {
				localError(c, 503, "暂时无法记录支付卡选择，尚未提交充值")
				return
			}
			chosen = id
			break
		}
	}
	if chosen == 0 {
		localError(c, 409, "卡密已锁定或指定卡片暂无可用项，请查询结果或联系商家")
		return
	}
	if e = tx.Commit(); e != nil {
		localError(c, 503, "暂时无法确认支付卡选择，尚未提交充值")
		return
	}
	r.CardID = chosen
	raw, e := cli.DirectOrder(c.Request.Context(), gin.H{"product": "gpt", "no_auto_card_switch": true, "card_id": r.CardID, "plan": r.Plan, "credential": req.Credential, "preflight_token": req.PF, "client_request_id": requestID, "pricing_version": r.Version}, requestID)
	var order struct {
		ID int64 `json:"id"`
	}
	if e != nil || json.Unmarshal(raw, &order) != nil || order.ID <= 0 {
		// Even 4xx can be ambiguous after upstream processing. Never unlock or invent
		// another request ID. Admin reconciliation is read-only against the real order.
		_, _ = db.DB.Exec("UPDATE local_cdks SET status='review',message='订单结果待核对，请联系商家；请勿再次付款' WHERE id=? AND status='reserved'", r.ID)
	} else {
		_, _ = db.DB.Exec("UPDATE local_cdks SET upstream_id=?,message='订单已受理，正在确认充值结果' WHERE id=? AND request_id=?", order.ID, r.ID, requestID)
	}
	r, _ = loadLocal("id", strconv.FormatInt(r.ID, 10))
	c.JSON(202, localPublicResult(r))
}
func localPublicResult(r localCode) gin.H {
	status := r.Status
	if status == "reserved" {
		status = "pending"
	}
	if status == "consumed" {
		status = "completed"
	}
	message := "兑换处理中，请稍后查询。"
	switch status {
	case "unused":
		message = "卡密尚未提交兑换"
	case "completed":
		message = "兑换已完成"
	case "failed":
		message = "本次兑换未完成；如需协助，请联系客服。"
	case "review":
		message = "订单需要商家核对，请勿重复提交。"
	}
	result := gin.H{
		"status":        status,
		"plan":          r.Plan,
		"email":         maskEmail(r.Email),
		"message":       message,
		"support_url":   mapleSupportURL,
		"redeem_locked": r.Status == "failed" || r.Status == "disabled",
	}
	if status == "completed" {
		result["upgrade_type"] = r.UpgradeType
		result["activated_at"] = r.ActivatedAt
		result["subscription_expires_at"] = r.SubscriptionExpiresAt
		result["expiry_estimated"] = r.ExpiryEstimated
	}
	return result
}

// localFailureAdminMessage uses the immutable record id and recognizable code
// prefix. Local CDKs retain only a hash after issuance, so the service never
// reconstructs or exposes a full secret that it no longer owns.
func localFailureAdminMessage(r localCode, upstreamStatus string, permanent bool) string {
	product, prefix := r.Plan, ""
	_ = db.DB.QueryRow(`SELECT COALESCE(p.name,c.plan),c.prefix
		FROM local_cdks c
		LEFT JOIN operations_product_bindings b ON b.local_id=c.id
		LEFT JOIN operations_products p ON p.id=COALESCE(b.product_id,'plus')
		WHERE c.id=?`, r.ID).Scan(&product, &prefix)
	contact := strings.TrimSpace(r.Email)
	if contact == "" {
		contact = "未取得；等待客户点击兑换页客服"
	}
	title, lockState := "待核对", "已锁定，等待人工核对；不会重复付款"
	if permanent {
		title = "失败"
		lockState = "已永久锁定，不能再次兑换或重新进入库存"
	}
	return fmt.Sprintf("🚨 兑换升级%s\n商品：%s\n购买卡密：%s…（站内编号 #%d）\n上游订单：%d\n上游状态：%s\n处理状态：%s\n\n客户联系方式\n• Telegram：暂未从商城同步（本条为备用提醒）\n• 邮箱：%s\n\n发给客户的商家客服（不是客户账号）：%s",
		title, product, prefix, r.ID, r.Upstream, upstreamStatus, lockState, contact, mapleSupportURL)
}
func refreshLocalOrder(c *gin.Context, r localCode) localCode {
	if r.Upstream <= 0 || (r.Status != "reserved" && r.Status != "review") {
		return r
	}
	now := time.Now().Unix()
	res, e := db.DB.Exec("UPDATE local_cdks SET last_checked=? WHERE id=? AND last_checked<?", now, r.ID, now-5)
	if e != nil {
		return r
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return r
	}
	raw, e := cardplatform.NewFromSettings().DirectOrderStatus(c.Request.Context(), r.Upstream)
	if e != nil {
		return r
	}
	var response struct {
		Order struct {
			ID           int64  `json:"id"`
			RequestID    string `json:"client_request_id"`
			Status       string `json:"status"`
			AccountEmail string `json:"account_email"`
			CompletedAt  string `json:"completed_at"`
		} `json:"order"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Order.ID != r.Upstream || response.Order.RequestID != r.Request {
		return r
	}
	status, message := "reserved", "订单处理中，请勿重复提交"
	switch response.Order.Status {
	case "completed":
		status, message = "consumed", "会员已开通，卡密已核销"
	case "failed_precharge", "cancelled":
		status, message = "failed", "升级未完成，该卡密已永久锁定，不能再次兑换。请点击联系客服处理。"
	case "declined", "failed", "requires_action":
		status, message = "review", "订单需要商家核对，请勿重复付款"
	}
	if status == "consumed" {
		e = recordAuthoritativeLocalCompletion(r.ID, message, response.Order.CompletedAt, now)
	} else {
		e = recordAuthoritativeLocalStatus(r.ID, status, message, now)
	}
	if e == nil {
		if outcomeErr := recordAutomationOutcome(r.ID, r.CardID, response.Order.Status, now); outcomeErr != nil {
			autoAlert("card_lifecycle", 0, "卡片成功率记录暂时失败；不会因此销卡，后台稍后继续核对。")
		} else if declineErr := recordAutomationDecline(r.ID, r.CardID, firstNonEmpty(response.Order.AccountEmail, r.Email), response.Order.Status, now); declineErr != nil {
			autoAlert("card_lifecycle", 0, "卡片拒付次数记录暂时失败；不会因此销卡，后台稍后继续核对。")
		} else {
			autoResolve("card_lifecycle")
		}
		r, _ = loadLocal("id", strconv.FormatInt(r.ID, 10))
		if status == "failed" {
			notifyLocalFailure(c.Request.Context(), autoOrderKey(r.ID), r, response.Order.Status, true)
		} else if status == "review" {
			notifyLocalFailure(c.Request.Context(), autoOrderKey(r.ID), r, response.Order.Status, false)
		}
	}
	return r
}
func LocalCDKResult(c *gin.Context) {
	var req struct {
		Token string `json:"redemption_token"`
	}
	if !localBody(c, &req) {
		return
	}
	r, ok := localToken(c, req.Token)
	if !ok {
		return
	}
	r = refreshLocalOrder(c, r)
	c.JSON(200, localPublicResult(r))
}
func LocalCDKReconcile(c *gin.Context) {
	var req struct {
		OrderID int64 `json:"order_id"`
	}
	if !localBody(c, &req) {
		return
	}
	r, e := loadLocal("id", c.Param("id"))
	if e == sql.ErrNoRows {
		localError(c, 404, "卡密不存在")
		return
	}
	if e != nil {
		localError(c, 500, "读取失败")
		return
	}
	if r.Request == "" || (r.Status != "reserved" && r.Status != "review") || req.OrderID <= 0 {
		localError(c, 409, "该卡密无待核对订单")
		return
	}
	raw, e := cardplatform.NewFromSettings().DirectOrderStatus(c.Request.Context(), req.OrderID)
	if e != nil {
		localError(c, 502, "无法读取上游订单")
		return
	}
	var v struct {
		Order struct {
			ID      int64  `json:"id"`
			Request string `json:"client_request_id"`
		} `json:"order"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Order.ID != req.OrderID || v.Order.Request != r.Request {
		localError(c, 409, "订单编号不属于此卡密，未做任何修改")
		return
	}
	if _, e = db.DB.Exec("UPDATE local_cdks SET upstream_id=?,last_checked=0 WHERE id=? AND request_id=?", req.OrderID, r.ID, r.Request); e != nil {
		localError(c, 500, "保存失败")
		return
	}
	r.Upstream = req.OrderID
	r = refreshLocalOrder(c, r)
	db.WriteAudit(c.GetString("username"), "local_cdk_reconcile", fmt.Sprintf("id=%d order=%d", r.ID, req.OrderID), c.ClientIP())
	adminResult := localPublicResult(r)
	adminResult["email"] = r.Email
	adminResult["message"] = r.Message
	c.JSON(200, adminResult)
}
