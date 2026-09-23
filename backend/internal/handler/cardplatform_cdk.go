package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

func writeCardErr(c *gin.Context, err error) {
	if ae, ok := err.(*cardplatform.APIError); ok {
		status := ae.HTTPStatus
		if status < 400 {
			status = http.StatusBadRequest
		}
		if status > 599 {
			status = http.StatusBadGateway
		}
		// 上游卡台 401/403 表示 API Key 无效/权限不足，不是 CDK 管理员会话失效。
		// 映射为 502，避免前端 authFetch 把 401 误判为登录过期并踢回登录页。
		upstreamAuth := status == http.StatusUnauthorized || status == http.StatusForbidden
		if upstreamAuth {
			status = http.StatusBadGateway
		}
		msg := ae.Msg
		if msg == "" {
			msg = "cardplatform api error"
		}
		if upstreamAuth && !strings.Contains(strings.ToLower(msg), "api key") {
			msg = msg + "（卡台 API Key 无效/未配置/无权限，与 CDK 登录无关）"
		}
		code := ae.ErrorCode
		if code == "" && upstreamAuth {
			code = "cardplatform_unauthorized"
		}
		c.JSON(status, gin.H{
			"error":      msg,
			"error_code": code,
			"code":       ae.Code,
			"upstream":   true,
		})
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "upstream": true})
}

// CardPlatformPlans GET /api/v1/admin/cardplatform/plans
// 实时套餐服务费（CDK 收费价）
func CardPlatformPlans(c *gin.Context) {
	cli := cardplatform.NewFromSettings()
	plans, err := cli.GetPlans(c.Request.Context())
	if err != nil {
		writeCardErr(c, err)
		return
	}
	// ★只下发「能发码也能兑换」的档位★——过滤在服务端，前端拿不到就渲染不出。
	// 放在前端过滤的话，每加一个界面就要记得再过滤一次；这次线上就是这么漏的：
	// 卡台透传了 ACC 的 claude_* 定价键，代理后台照单全列，还能发码。
	sellable := plans.SellablePlans()
	m := map[string]cardplatform.SellablePlan{}
	for _, p := range sellable {
		m[p.Key] = p
	}
	c.JSON(http.StatusOK, gin.H{
		"version": plans.Version,
		"plans":   m,
		"base":    cardplatform.LoadConfig().SiteBase,
		// 展示顺序/文案/性质仍以卡台注册表为准，前端不维护档位清单
		"registry": sellable,
	})
}

// CardPlatformBalance GET /api/v1/admin/cardplatform/balance
func CardPlatformBalance(c *gin.Context) {
	cli := cardplatform.NewFromSettings()
	bal, err := cli.GetBalance(c.Request.Context())
	if err != nil {
		writeCardErr(c, err)
		return
	}
	c.JSON(http.StatusOK, bal)
}

// CardPlatformIssueCDKs POST /api/v1/admin/cardplatform/cdks
// body: { plan, count, funding_confirmed }
func CardPlatformIssueCDKs(c *gin.Context) {
	var req struct {
		Plan             string `json:"plan"`
		Count            int    `json:"count"`
		FundingConfirmed bool   `json:"funding_confirmed"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	plan := strings.TrimSpace(req.Plan)
	if plan == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "plan is required"})
		return
	}
	// ★档位以卡台实时下发为准，不要在这里写死白名单★
	// 写死的话，卡台每开一个新档位（Codex 点数 credit250/500/1000）这里都会 400 挡下，
	// 代理明明有权限发码却发不出来，而且报错还指向一个过期的档位清单。
	//
	// 但「实时下发」≠「定价表里有就能卖」：卡台透传的是 ACC 的整张定价表，
	// 里面有 claude_*（没有 CDK 兑换流程）也有 enabled=false 的档（兑换时被 ACC 挡）。
	// 按 SellableKeys 校验，跟界面上能看到的是同一份，不会出现「看得见发不出」
	// 或者「发得出兑不掉」。
	if cli := cardplatform.NewFromSettings(); cli != nil {
		if plans, err := cli.GetPlans(c.Request.Context()); err == nil && plans != nil && len(plans.Plans) > 0 {
			sellable := plans.SellableKeys()
			if len(sellable) > 0 && !sellable[plan] {
				known := make([]string, 0, len(sellable))
				for k := range sellable {
					known = append(known, k)
				}
				sort.Strings(known)
				reason := "unknown plan"
				if _, exists := plans.Plans[plan]; exists {
					reason = "plan not open for CDK（卡台未开放本档位，或 ACC 定价里是停用状态）"
				}
				c.JSON(http.StatusBadRequest, gin.H{"error": reason + ": " + plan + "; available: " + strings.Join(known, " | ")})
				return
			}
		}
		// 拿不到档位表（卡台不可用/未配置）时不拦：交给卡台发码时校验，
		// 避免因为一次网络抖动就让代理发不了码。
	}
	if !req.FundingConfirmed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "funding_confirmed must be true（确认承担兑换时开卡/充值/订阅实付）"})
		return
	}
	if req.Count < 1 {
		req.Count = 1
	}
	if req.Count > cardplatform.MaxIssueCount {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("count max %d", cardplatform.MaxIssueCount)})
		return
	}

	cli := cardplatform.NewFromSettings()
	idem := c.GetHeader("Idempotency-Key")
	if idem == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		idem = "cdk-issue-" + hex.EncodeToString(b)
	}
	// 本站选卡配置 → 发码偏好（跳过未启动卡头；ch1 等历史写法会归一成 one）
	var issuePrefs []cardplatform.IssueCardPref
	if pref, ok := issuePrefFromSite(); ok {
		issuePrefs = append(issuePrefs, pref)
	}
	var res *cardplatform.IssueCDKResult
	var err error
	if len(issuePrefs) > 0 {
		res, err = cli.IssueCDKs(c.Request.Context(), plan, req.Count, idem, issuePrefs[0])
	} else {
		res, err = cli.IssueCDKs(c.Request.Context(), plan, req.Count, idem)
	}
	if err != nil {
		// 超时/断线时卡台可能已经出码。立刻从卡台列表把完整码捞回本站。
		if recovered, recErr := cli.SyncUpstreamFullCodes(c.Request.Context(), "unused", plan, 10); recErr == nil && recovered != nil && recovered.Imported > 0 {
			c.JSON(http.StatusOK, gin.H{
				"requested":     req.Count,
				"issued":        recovered.Codes,
				"count":         len(recovered.Codes),
				"stored_count":  recovered.Imported,
				"store_failed":  0,
				"server_stored": true,
				"recovered":     true,
				"msg":           fmt.Sprintf("发码请求未完成，已从卡台找回 %d 张完整码", recovered.Imported),
			})
			return
		}
		writeCardErr(c, err)
		return
	}
	u, _ := c.Get("username")
	username, _ := u.(string)
	prefNote := ""
	if len(issuePrefs) > 0 {
		prefNote = " pref=" + issuePrefs[0].Issuer + "/" + issuePrefs[0].SegmentKey
	}
	db.WriteAudit(username, "cardplatform_issue_cdk", "plan="+plan+" count="+strconv.Itoa(req.Count)+prefNote, c.ClientIP())
	// 规范化：保证前端总能拿到完整 code 字段；绝不把 code_prefix 填进 code
	issued := make([]gin.H, 0, len(res.Issued))
	stored, storeFailed := 0, 0
	for _, it := range res.Issued {
		code := strings.TrimSpace(it.Code)
		prefix := strings.TrimSpace(it.CodePrefix)
		if code == "" {
			// 防御上游异常：只回了前缀时仍原样暴露前缀字段，但不伪造 code
			issued = append(issued, gin.H{
				"id": it.ID, "code": "", "plan": it.Plan,
				"code_prefix": prefix, "fee_amount_minor": it.FeeAmountMinor,
				"incomplete": true, "stored": false,
			})
			continue
		}
		if prefix == "" && len(code) >= 14 {
			prefix = code[:14]
		}
		// 本站 SQLite 持久化完整码（卡台列表只回 prefix）
		storedOK := false
		if err := db.SaveCardplatformCDKCode(it.ID, code, prefix, it.Plan, it.FeeAmountMinor); err != nil {
			storeFailed++
			log.Printf("[cdk-issue] save full code failed id=%d prefix=%s: %v", it.ID, prefix, err)
		} else {
			stored++
			storedOK = true
		}
		issued = append(issued, gin.H{
			"id": it.ID, "code": code, "plan": it.Plan,
			"code_prefix": prefix, "fee_amount_minor": it.FeeAmountMinor,
			"code_length":   len(code),
			"full_code":     code,
			"stored":        storedOK,
			"has_full_code": true,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"requested":     res.Requested,
		"issued":        issued,
		"count":         len(issued),
		"stored_count":  stored,
		"store_failed":  storeFailed,
		"server_stored": true,
	})
}

// CardPlatformSyncUpstreamCDKs POST /api/v1/admin/cardplatform/cdks/sync
// 从卡台列表拉取完整码写入本站。卡台升级后列表会带 code；旧后端只能拿到前缀。
func CardPlatformSyncUpstreamCDKs(c *gin.Context) {
	var req struct {
		Status   string `json:"status"`
		Plan     string `json:"plan"`
		MaxPages int    `json:"max_pages"`
	}
	_ = c.ShouldBindJSON(&req)
	cli := cardplatform.NewFromSettings()
	res, err := cli.SyncUpstreamFullCodes(c.Request.Context(), req.Status, req.Plan, req.MaxPages)
	if err != nil {
		writeCardErr(c, err)
		return
	}
	u, _ := c.Get("username")
	username, _ := u.(string)
	db.WriteAudit(username, "cardplatform_sync_cdk",
		fmt.Sprintf("imported=%d prefix_only=%d scanned=%d", res.Imported, res.PrefixOnly, res.Scanned), c.ClientIP())
	msg := fmt.Sprintf("从卡台同步：新入库 %d 张，扫描 %d 张", res.Imported, res.Scanned)
	if res.NeedCardplatformUpgrade {
		msg = "卡台列表仍只返回前缀。请先升级卡台后端后再同步。"
	}
	c.JSON(http.StatusOK, gin.H{
		"imported":                  res.Imported,
		"updated":                   res.Updated,
		"prefix_only":               res.PrefixOnly,
		"scanned":                   res.Scanned,
		"pages":                     res.Pages,
		"upstream_total":            res.UpstreamTotal,
		"need_cardplatform_upgrade": res.NeedCardplatformUpgrade,
		"codes":                     res.Codes,
		"full_code_in_store":        db.CountCardplatformCDKCodes(),
		"msg":                       msg,
	})
}

// CardPlatformStoreCDKCodes POST /api/v1/admin/cardplatform/cdks/store
// 把完整码写入本站 SQLite（发码时自动写；也可用本机缓存/导出回填历史码）。
// body: { items: [{ id, code, code_prefix?, plan?, fee_amount_minor? }] }
func CardPlatformStoreCDKCodes(c *gin.Context) {
	var req struct {
		Items []struct {
			ID             int64  `json:"id"`
			Code           string `json:"code"`
			CodePrefix     string `json:"code_prefix"`
			Plan           string `json:"plan"`
			FeeAmountMinor int64  `json:"fee_amount_minor"`
		} `json:"items"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Items) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "items required"})
		return
	}
	if len(req.Items) > 500 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "items max 500"})
		return
	}
	saved, skipped, failed := 0, 0, 0
	for _, it := range req.Items {
		code := strings.TrimSpace(it.Code)
		if len(code) < 20 || !strings.Contains(code, "-") {
			skipped++
			continue
		}
		prefix := strings.TrimSpace(it.CodePrefix)
		if prefix == "" && len(code) >= 14 {
			prefix = code[:14]
		}
		if err := db.SaveCardplatformCDKCode(it.ID, code, prefix, it.Plan, it.FeeAmountMinor); err != nil {
			failed++
			log.Printf("[cdk-store] save failed id=%d: %v", it.ID, err)
			continue
		}
		saved++
	}
	u, _ := c.Get("username")
	username, _ := u.(string)
	db.WriteAudit(username, "cardplatform_store_cdk",
		"saved="+strconv.Itoa(saved)+" skipped="+strconv.Itoa(skipped)+" failed="+strconv.Itoa(failed), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{
		"ok": true, "saved": saved, "skipped": skipped, "failed": failed,
	})
}

// CardPlatformListStoredCDKs GET /api/v1/admin/cardplatform/cdks/stored
// 只读本站已存完整码（随时复制/导出；不依赖卡台列表）。
// query: plan= / q= / status= / page= / page_size= / limit= / format=json|txt
//
// 列表默认分页（page_size=20）；导出 txt / 显式大 limit 仍可一次拉多条。
// 状态：本站 SQLite + 当前页轻量向卡台核对（不再整库翻页）；Webhook completed 也会回写 consumed。
func CardPlatformListStoredCDKs(c *gin.Context) {
	plan := strings.TrimSpace(c.Query("plan"))
	q := strings.TrimSpace(c.Query("q"))
	status := strings.TrimSpace(strings.ToLower(c.Query("status")))
	format := strings.ToLower(strings.TrimSpace(c.DefaultQuery("format", "json")))
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "0"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "0"))
	skipSync := c.Query("sync") == "0" || c.Query("sync") == "false"

	// 导出：一次尽量多取（硬顶 10000），按本地 status 过滤
	if format == "txt" || format == "text" || format == "plain" {
		if limit <= 0 {
			limit = 10000
		}
		list, _, err := db.ListCardplatformStoredCDKCodesPage(plan, q, status, 1, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		var b strings.Builder
		for _, it := range list {
			if strings.TrimSpace(it.Code) == "" {
				continue
			}
			b.WriteString(it.Code)
			b.WriteByte('\n')
		}
		c.Header("Content-Disposition", `attachment; filename="cdk-full-codes.txt"`)
		c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(b.String()))
		return
	}

	// JSON 列表：优先 page/page_size；兼容旧客户端只传 limit（复制全部等 bulk）
	hasPageParam := strings.TrimSpace(c.Query("page")) != "" || strings.TrimSpace(c.Query("page_size")) != ""
	bulkLegacy := !hasPageParam && limit > 0
	if pageSize <= 0 {
		if bulkLegacy {
			pageSize = limit
			page = 1
		} else {
			pageSize = 20
		}
	}
	maxPage := 200
	if bulkLegacy {
		maxPage = 10000 // 复制/导出全部仍可一次取够
	}
	if pageSize > maxPage {
		pageSize = maxPage
	}
	if page < 1 {
		page = 1
	}

	list, total, err := db.ListCardplatformStoredCDKCodesPage(plan, q, status, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 当前页向卡台核对状态（仅本页 ID；bulk 大导出跳过以免拖慢）
	statusMap := map[int64]string{}
	if !skipSync && !bulkLegacy && len(list) > 0 && len(list) <= 200 {
		ids := make([]int64, 0, len(list))
		for _, it := range list {
			if it.UpstreamID > 0 {
				ids = append(ids, it.UpstreamID)
			}
		}
		statusMap = refreshStoredCDKStatuses(c.Request.Context(), ids)
	}

	noteIDs := make([]int64, 0, len(list))
	for _, it := range list {
		noteIDs = append(noteIDs, it.UpstreamID)
	}
	notes := db.MapCardplatformCDKNotes(noteIDs)
	out := make([]gin.H, 0, len(list))
	for _, it := range list {
		st := it.Status
		if s, ok := statusMap[it.UpstreamID]; ok && s != "" {
			st = s
		}
		if st == "" {
			st = "unused"
		}
		out = append(out, gin.H{
			"id": it.UpstreamID, "code": it.Code, "full_code": it.Code,
			"code_prefix": it.CodePrefix, "plan": it.Plan, "status": st,
			"fee_amount_minor": it.FeeAmountMinor, "created_at": it.CreatedAt,
			"has_full_code": true, "stored": true,
			"note": notes[it.UpstreamID],
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"list":               out,
		"total":              total,
		"page":               page,
		"page_size":          pageSize,
		"full_code_in_store": db.CountCardplatformCDKCodes(),
		"server_stored":      true,
		"status_synced":      len(statusMap) > 0,
	})
}

// refreshStoredCDKStatuses 按上游 id 轻量查询卡台状态并回写本站缓存。
// 每页并发有限，避免整库扫描。
func refreshStoredCDKStatuses(ctx context.Context, ids []int64) map[int64]string {
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out
	}
	cli := cardplatform.NewFromSettings()
	type pair struct {
		id int64
		st string
	}
	ch := make(chan pair, len(ids))
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for _, id := range ids {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, err := cli.ListCDKsQuery(ctx, cardplatform.CDKListQuery{
				Page: 1, PageSize: 5, Query: strconv.FormatInt(id, 10),
			})
			if err != nil || res == nil {
				return
			}
			for _, it := range res.List {
				if it.ID == id && strings.TrimSpace(it.Status) != "" {
					st := strings.ToLower(strings.TrimSpace(it.Status))
					_ = db.UpdateCardplatformCDKStatus(id, st)
					ch <- pair{id: id, st: st}
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(ch)
	}()
	for p := range ch {
		out[p.id] = p.st
	}
	return out
}

// CardPlatformDisableCDK POST /api/v1/admin/cardplatform/cdks/:id/disable
func CardPlatformDisableCDK(c *gin.Context) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	cli := cardplatform.NewFromSettings()
	if err := cli.DisableCDK(c.Request.Context(), id); err != nil {
		writeCardErr(c, err)
		return
	}
	_ = db.UpdateCardplatformCDKStatus(id, "disabled")
	u, _ := c.Get("username")
	username, _ := u.(string)
	db.WriteAudit(username, "cardplatform_disable_cdk", "id="+strconv.FormatInt(id, 10), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": id, "status": "disabled"})
}

// CardPlatformEnableCDK POST /api/v1/admin/cardplatform/cdks/:id/enable — 解除禁用
func CardPlatformEnableCDK(c *gin.Context) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	cli := cardplatform.NewFromSettings()
	if err := cli.EnableCDK(c.Request.Context(), id); err != nil {
		writeCardErr(c, err)
		return
	}
	_ = db.UpdateCardplatformCDKStatus(id, "unused")
	u, _ := c.Get("username")
	username, _ := u.(string)
	db.WriteAudit(username, "cardplatform_enable_cdk", "id="+strconv.FormatInt(id, 10), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": id, "status": "unused"})
}

// CardPlatformBatchDisableCDKs POST /api/v1/admin/cardplatform/cdks/batch-disable
// body: { ids: [1,2,3] }
func CardPlatformBatchDisableCDKs(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.IDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ids required"})
		return
	}
	if len(req.IDs) > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ids max 100"})
		return
	}
	cli := cardplatform.NewFromSettings()
	res, err := cli.BatchDisableCDKs(c.Request.Context(), req.IDs)
	if err != nil {
		writeCardErr(c, err)
		return
	}
	for _, id := range res.Disabled {
		_ = db.UpdateCardplatformCDKStatus(id, "disabled")
	}
	u, _ := c.Get("username")
	username, _ := u.(string)
	db.WriteAudit(username, "cardplatform_batch_disable_cdk",
		"ok="+strconv.Itoa(res.DisabledCount)+" fail="+strconv.Itoa(res.FailedCount), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{
		"ok":       true,
		"disabled": res.Disabled, "failed": res.Failed,
		"disabled_count": res.DisabledCount, "failed_count": res.FailedCount,
	})
}

// CardPlatformBatchEnableCDKs POST /api/v1/admin/cardplatform/cdks/batch-enable
func CardPlatformBatchEnableCDKs(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.IDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ids required"})
		return
	}
	if len(req.IDs) > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ids max 100"})
		return
	}
	cli := cardplatform.NewFromSettings()
	res, err := cli.BatchEnableCDKs(c.Request.Context(), req.IDs)
	if err != nil {
		writeCardErr(c, err)
		return
	}
	for _, id := range res.Enabled {
		_ = db.UpdateCardplatformCDKStatus(id, "unused")
	}
	u, _ := c.Get("username")
	username, _ := u.(string)
	db.WriteAudit(username, "cardplatform_batch_enable_cdk",
		"ok="+strconv.Itoa(res.EnabledCount)+" fail="+strconv.Itoa(res.FailedCount), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"enabled": res.Enabled, "failed": res.Failed,
		"enabled_count": res.EnabledCount, "failed_count": res.FailedCount,
	})
}

// CardPlatformListCDKs GET /api/v1/admin/cardplatform/cdks?page=&page_size=&q=&status=&plan=
func CardPlatformListCDKs(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	ps, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	cli := cardplatform.NewFromSettings()
	res, err := cli.ListCDKsQuery(c.Request.Context(), cardplatform.CDKListQuery{
		Page: page, PageSize: ps,
		Status: c.Query("status"), Plan: c.Query("plan"), Query: c.Query("q"),
	})
	if err != nil {
		writeCardErr(c, err)
		return
	}
	// 用本站发码缓存补全 code，便于列表点击复制完整码
	type rowOut struct {
		ID             int64  `json:"id"`
		Plan           string `json:"plan"`
		CodePrefix     string `json:"code_prefix"`
		Status         string `json:"status"`
		FeeAmountMinor int64  `json:"fee_amount_minor"`
		CreatedAt      string `json:"created_at"`
		Code           string `json:"code,omitempty"`
		FullCode       string `json:"full_code,omitempty"`
		HasFullCode    bool   `json:"has_full_code"`
		Note           string `json:"note,omitempty"`
	}
	ids := make([]int64, 0, len(res.List))
	for _, it := range res.List {
		ids = append(ids, it.ID)
	}
	notes := db.MapCardplatformCDKNotes(ids)
	out := make([]rowOut, 0, len(res.List))
	withFull := 0
	for _, it := range res.List {
		full, ok := db.LookupCardplatformCDKCode(it.ID, it.CodePrefix)
		if !ok {
			if up := it.FullCodeText(); up != "" {
				full, ok = up, true
				_ = db.SaveCardplatformCDKCodeWithStatus(it.ID, up, it.CodePrefix, it.Plan, it.FeeAmountMinor, it.Status)
			}
		}
		row := rowOut{
			ID: it.ID, Plan: it.Plan, CodePrefix: it.CodePrefix, Status: it.Status,
			FeeAmountMinor: it.FeeAmountMinor, CreatedAt: it.CreatedAt,
			HasFullCode: ok, Note: notes[it.ID],
		}
		if ok {
			row.Code = full
			row.FullCode = full
			withFull++
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{
		"list":               out,
		"total":              res.Total,
		"full_code_on_page":  withFull,
		"full_code_in_store": db.CountCardplatformCDKCodes(),
		"server_stored":      true,
	})
}

// CardPlatformListCDKOrders GET /api/v1/admin/cardplatform/cdk-orders
// 对账列表：卡台 CDK 兑换订单；若上游暂未带 code_prefix/cdk_status，则本站按 cdk_id 补齐。
// 支持 page / page_size(1–100) / status / cdk_id / order_id。
func CardPlatformListCDKOrders(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	ps, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if ps < 1 {
		ps = 20
	}
	if ps > 100 {
		ps = 100
	}
	q := cardplatform.CDKOrderListQuery{
		Page:     page,
		PageSize: ps,
		Status:   strings.TrimSpace(c.Query("status")),
		CDKID:    int64Any(c.Query("cdk_id")),
		OrderID:  int64Any(c.Query("order_id")),
	}
	cli := cardplatform.NewFromSettings()
	raw, err := cli.ListCDKOrdersQuery(c.Request.Context(), q)
	if err != nil {
		writeCardErr(c, err)
		return
	}
	if len(raw) == 0 {
		c.JSON(http.StatusOK, gin.H{"list": []any{}, "total": 0, "page": page, "page_size": ps})
		return
	}
	enriched := enrichCDKOrderList(c, cli, raw)
	enriched["page"] = page
	enriched["page_size"] = ps
	c.JSON(http.StatusOK, enriched)
}

// CardPlatformDeleteCard DELETE /api/v1/admin/cardplatform/cards/:id
// 代理卡台 DELETE /cards/{id}：永久删卡并把卡内余额退回平台余额。
func CardPlatformDeleteCard(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "card id required"})
		return
	}
	cli := cardplatform.NewFromSettings()
	raw, err := cli.DeleteCard(c.Request.Context(), id)
	if err != nil {
		writeCardErr(c, err)
		return
	}
	u, _ := c.Get("username")
	username, _ := u.(string)
	db.WriteAudit(username, "cardplatform_delete_card", "card_id="+id, c.ClientIP())
	// 上游 data 可能为空（仅 msg），统一给前端可消费结构
	out := gin.H{"ok": true, "card_id": id, "message": "删卡成功，余额已退回"}
	if len(raw) > 0 && string(raw) != "null" {
		var m any
		if json.Unmarshal(raw, &m) == nil && m != nil {
			out["data"] = m
		}
	}
	c.JSON(http.StatusOK, out)
}

// CardPlatformGetCDKOrder GET /api/v1/admin/cardplatform/cdk-orders/:id
func CardPlatformGetCDKOrder(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}
	cli := cardplatform.NewFromSettings()
	raw, err := cli.GetCDKOrder(c.Request.Context(), id)
	if err != nil {
		writeCardErr(c, err)
		return
	}
	if len(raw) == 0 {
		c.JSON(http.StatusOK, gin.H{})
		return
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		c.Data(http.StatusOK, "application/json; charset=utf-8", raw)
		return
	}
	enrichOneCDKOrder(c, cli, m)
	c.JSON(http.StatusOK, m)
}

// enrichCDKOrderList 解析 list/total，按需从列码接口补 code_prefix / cdk_status。
func enrichCDKOrderList(c *gin.Context, cli *cardplatform.Client, raw json.RawMessage) gin.H {
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		// 无法解析时原样包一层，避免吞数据
		return gin.H{"list": []any{}, "total": 0, "parse_error": true}
	}
	listAny, _ := envelope["list"].([]any)
	total := int64Any(envelope["total"])
	// 兜底：上游 total 缺失时至少不低于本页条数，避免前端「共 0 笔」把下一页锁死
	if total <= 0 && len(listAny) > 0 {
		total = int64(len(listAny))
	}
	// 收集需要补全的 cdk_id
	needCDK := false
	ids := map[int64]bool{}
	for _, it := range listAny {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if strAny(m["code_prefix"]) == "" || strAny(m["cdk_status"]) == "" {
			needCDK = true
		}
		if id := int64Any(m["cdk_id"]); id > 0 {
			ids[id] = true
		}
	}
	cdkMap := map[int64]cardplatform.CDKListItem{}
	if needCDK && len(ids) > 0 {
		// 拉若干页 CDK 建索引（白标量级通常不大）
		for page := 1; page <= 20; page++ {
			res, err := cli.ListCDKs(c.Request.Context(), page, 100)
			if err != nil || res == nil || len(res.List) == 0 {
				break
			}
			for _, item := range res.List {
				cdkMap[item.ID] = item
			}
			if len(res.List) < 100 || len(cdkMap) >= res.Total {
				break
			}
		}
	}
	outList := make([]any, 0, len(listAny))
	for _, it := range listAny {
		m, ok := it.(map[string]any)
		if !ok {
			outList = append(outList, it)
			continue
		}
		if id := int64Any(m["cdk_id"]); id > 0 {
			if item, ok := cdkMap[id]; ok {
				if strAny(m["code_prefix"]) == "" {
					m["code_prefix"] = item.CodePrefix
				}
				if strAny(m["cdk_status"]) == "" {
					m["cdk_status"] = item.Status
				}
			}
		}
		// 归一化 CDK 生命周期展示字段
		m["cdk_lifecycle"] = cdkLifecycleLabel(strAny(m["cdk_status"]), strAny(m["status"]), strAny(m["service_fee_status"]))
		outList = append(outList, m)
	}
	return gin.H{"list": outList, "total": total}
}

func enrichOneCDKOrder(c *gin.Context, cli *cardplatform.Client, m map[string]any) {
	id := int64Any(m["cdk_id"])
	if id > 0 && (strAny(m["code_prefix"]) == "" || strAny(m["cdk_status"]) == "") {
		for page := 1; page <= 20; page++ {
			res, err := cli.ListCDKs(c.Request.Context(), page, 100)
			if err != nil || res == nil {
				break
			}
			found := false
			for _, item := range res.List {
				if item.ID == id {
					if strAny(m["code_prefix"]) == "" {
						m["code_prefix"] = item.CodePrefix
					}
					if strAny(m["cdk_status"]) == "" {
						m["cdk_status"] = item.Status
					}
					found = true
					break
				}
			}
			if found || len(res.List) < 100 {
				break
			}
		}
	}
	m["cdk_lifecycle"] = cdkLifecycleLabel(strAny(m["cdk_status"]), strAny(m["status"]), strAny(m["service_fee_status"]))
}

func cdkLifecycleLabel(cdkStatus, orderStatus, feeStatus string) string {
	switch strings.ToLower(strings.TrimSpace(cdkStatus)) {
	case "consumed":
		return "已消耗"
	case "reserved":
		return "预留中"
	case "unused":
		if strings.EqualFold(feeStatus, "released") ||
			strings.EqualFold(orderStatus, "declined") ||
			strings.EqualFold(orderStatus, "failed_precharge") ||
			strings.EqualFold(orderStatus, "cancelled") {
			return "已释放"
		}
		return "未使用"
	case "frozen":
		return "已冻结"
	case "disabled":
		return "已禁用"
	}
	if strings.EqualFold(feeStatus, "released") {
		return "服务费已释放"
	}
	if cdkStatus != "" {
		return cdkStatus
	}
	return "—"
}

func strAny(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}

func int64Any(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n
	default:
		return 0
	}
}

// ---- 公开兑换 BFF（浏览器只打本站，本站转发卡台）----

func deviceFrom(c *gin.Context) string {
	if d := strings.TrimSpace(c.GetHeader("X-Redemption-Device")); d != "" {
		return d
	}
	return c.GetHeader("User-Agent")
}

func proxyPublicJSON(c *gin.Context, status int, raw json.RawMessage) {
	if len(raw) == 0 {
		c.Status(status)
		return
	}
	c.Data(status, "application/json; charset=utf-8", raw)
}

func publicResultString(payload map[string]any, key string) string {
	// Envelope-level status often only means the HTTP request succeeded. The
	// actual redemption state belongs to the innermost order and must win.
	if data, ok := payload["data"].(map[string]any); ok {
		if order, ok := data["order"].(map[string]any); ok {
			if value := strAny(order[key]); value != "" {
				return value
			}
		}
	}
	if order, ok := payload["order"].(map[string]any); ok {
		if value := strAny(order[key]); value != "" {
			return value
		}
	}
	if data, ok := payload["data"].(map[string]any); ok {
		if value := strAny(data[key]); value != "" {
			return value
		}
	}
	return strAny(payload[key])
}

func extractPublicResultEmail(payload map[string]any) string {
	if email := publicResultString(payload, "account_email"); email != "" {
		return email
	}
	for _, container := range []any{payload["order"], payload["data"]} {
		if m, ok := container.(map[string]any); ok {
			if email := strAny(m["account_email"]); email != "" {
				return email
			}
			if order, ok := m["order"].(map[string]any); ok {
				if email := strAny(order["account_email"]); email != "" {
					return email
				}
			}
		}
	}
	return ""
}

func extractCredentialEmail(v any) string {
	credential, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	return strings.TrimSpace(strAny(credential["email"]))
}

func safePublicStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "queued", "pending", "processing", "running", "submitted", "review":
		return strings.ToLower(strings.TrimSpace(raw))
	case "completed", "success", "done", "paid":
		return "completed"
	case "failed", "failed_precharge", "declined", "cancelled", "error":
		return "failed"
	default:
		return "processing"
	}
}

func publicResultBool(payload map[string]any, key string) (bool, bool) {
	if data, ok := payload["data"].(map[string]any); ok {
		if order, ok := data["order"].(map[string]any); ok {
			if value, ok := order[key].(bool); ok {
				return value, true
			}
		}
	}
	if order, ok := payload["order"].(map[string]any); ok {
		if value, ok := order[key].(bool); ok {
			return value, true
		}
	}
	if data, ok := payload["data"].(map[string]any); ok {
		if value, ok := data[key].(bool); ok {
			return value, true
		}
	}
	value, ok := payload[key].(bool)
	return value, ok
}

func publicResultAllowsRetry(raw json.RawMessage) bool {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil || payload == nil {
		return false
	}
	canResubmit, explicit := publicResultBool(payload, "can_resubmit")
	if !explicit || !canResubmit {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(publicResultString(payload, "status"))) {
	case "failed", "failed_precharge", "declined", "cancelled", "error":
		return true
	default:
		return false
	}
}

func safePublicStage(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "queued", "credential_check", "pricing", "checkout", "payment",
		"subscription", "invoice", "renewal", "reconcile", "completed":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return ""
	}
}

func publicResultMessage(status string) string {
	switch safePublicStatus(status) {
	case "completed":
		return "兑换已完成"
	case "failed":
		return "本次兑换未完成；如需协助，请联系客服。"
	default:
		return "兑换处理中，请稍后查询。"
	}
}

func safePublicEventStep(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "queued", "credential_check", "pricing", "checkout", "payment",
		"subscription", "invoice", "renewal", "reconcile", "completed":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func safePublicEventCategory(value, status string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "success", "completed":
		return "success"
	case "failed", "error":
		return "error"
	case "warning":
		return "warning"
	case "info", "pending":
		return "info"
	}
	switch safePublicStatus(status) {
	case "completed":
		return "success"
	case "failed":
		return "error"
	default:
		return "info"
	}
}

func publicEventMessage(step, status string) string {
	if safePublicStatus(status) == "failed" {
		return "该步骤未完成；如需协助，请联系客服。"
	}
	if safePublicStatus(status) == "completed" || step == "completed" {
		return "该步骤已完成。"
	}
	switch step {
	case "queued":
		return "订单已受理。"
	case "credential_check":
		return "账号信息已核验。"
	case "pricing":
		return "正在确认订单金额。"
	case "checkout":
		return "正在准备支付。"
	case "payment":
		return "正在确认支付结果。"
	case "subscription":
		return "正在确认订阅状态。"
	case "invoice":
		return "正在核对本次账单。"
	case "renewal":
		return "正在处理续费。"
	case "reconcile":
		return "正在核对处理结果。"
	default:
		return "订单处理中。"
	}
}

// safePublicResultPayload is an allowlist. Public polling never receives the
// upstream token, raw provider identifiers, card details or internal notes.
func safePublicResultPayload(payload map[string]any) map[string]any {
	status := safePublicStatus(publicResultString(payload, "status"))
	stage := safePublicStage(publicResultString(payload, "stage"))
	out := map[string]any{
		"status":  status,
		"message": publicResultMessage(status),
	}
	if stage != "" {
		out["stage"] = stage
	}

	var order map[string]any
	if data, ok := payload["data"].(map[string]any); ok {
		if candidate, ok := data["order"].(map[string]any); ok {
			order = candidate
		} else if _, hasTopOrder := payload["order"].(map[string]any); !hasTopOrder {
			order = data
		}
	}
	if order == nil {
		if candidate, ok := payload["order"].(map[string]any); ok {
			order = candidate
		}
	}
	if order != nil {
		safeOrder := map[string]any{}
		safeOrder["status"] = status
		if stage != "" {
			safeOrder["stage"] = stage
		}
		if plan := strAny(order["plan"]); plan != "" && len(plan) <= 64 {
			safeOrder["plan"] = plan
		}
		for _, key := range []string{"created_at", "updated_at", "completed_at"} {
			switch value := order[key].(type) {
			case string:
				if len(value) <= 64 {
					safeOrder[key] = value
				}
			case float64, int64, int:
				safeOrder[key] = value
			}
		}
		if email := maskEmail(strAny(order["account_email"])); email != "" {
			safeOrder["account_email"] = email
		}
		if len(safeOrder) > 0 {
			out["order"] = safeOrder
		}
	}
	if email := maskEmail(extractPublicResultEmail(payload)); email != "" {
		out["account_email"] = email
	}

	var rawEvents []any
	if events, ok := payload["events"].([]any); ok {
		rawEvents = events
	} else if data, ok := payload["data"].(map[string]any); ok {
		if events, ok := data["events"].([]any); ok {
			rawEvents = events
		}
	}
	if len(rawEvents) > 0 {
		events := make([]map[string]any, 0, len(rawEvents))
		for _, item := range rawEvents {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			step := safePublicEventStep(strAny(m["step"]))
			if step == "" {
				// An event with only a timestamp is not useful to the customer and
				// must not become an empty row in the public progress timeline.
				continue
			}
			rawStatus := strAny(m["to_status"])
			if rawStatus == "" {
				rawStatus = strAny(m["status"])
			}
			safeStatus := safePublicStatus(rawStatus)
			safeEvent := map[string]any{
				"step":           step,
				"category":       safePublicEventCategory(strAny(m["category"]), rawStatus),
				"to_status":      safeStatus,
				"public_message": publicEventMessage(step, rawStatus),
			}
			switch created := m["created_at"].(type) {
			case string:
				if len(created) <= 64 {
					safeEvent["created_at"] = created
				}
			case float64, int64, int:
				safeEvent["created_at"] = created
			}
			if len(safeEvent) > 0 {
				events = append(events, safeEvent)
			}
		}
		if len(events) > 0 {
			out["events"] = events
		}
	}
	return out
}

// PublicCDKPreview POST /api/v1/public/cdk/preview
func PublicCDKPreview(c *gin.Context) {
	privateNoStore(c)
	var body map[string]any
	if !localBody(c, &body) {
		return
	}
	code := normalizePublicCDK(str(body["code"]))
	if !validPublicCDK(code) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "卡密格式无效"})
		return
	}
	// Capture the local state before the network request. The successful
	// upstream response may only be persisted if no concurrent redeem or newer
	// preview changed this exact binding in the meantime.
	snapshot, err := db.GetBindingByCDK(code)
	if err != nil {
		log.Printf("[cdk-preview] binding snapshot failed: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂时无法安全验证卡密，请稍后重试。"})
		return
	}
	cli := cardplatform.NewFromSettings()
	allowFailedRetry := false
	nowUnix := time.Now().UTC().Unix()
	if snapshot != nil && (snapshot.QueryStartedAt > 0 || snapshot.QueryExpiresAt > 0) &&
		(snapshot.QueryExpiresAt <= 0 || nowUnix < snapshot.QueryExpiresAt) {
		// An active attempt may be replaced only after the current token proves
		// that its upstream order is terminally failed. Processing, successful,
		// unknown and unavailable states remain locked against duplicate payment.
		if strings.TrimSpace(snapshot.RedemptionToken) == "" {
			c.JSON(http.StatusConflict, gin.H{"error": "该卡密已有处理中记录，请使用卡密查询查看进度。"})
			return
		}
		resultStatus, resultRaw, resultErr := cli.Result(c.Request.Context(), snapshot.RedemptionToken, deviceFrom(c))
		if resultErr != nil || resultStatus < 200 || resultStatus >= 300 || !publicResultAllowsRetry(resultRaw) {
			c.JSON(http.StatusConflict, gin.H{"error": "该卡密已提交兑换；仅在确认本次失败后才能重新验证，请先使用卡密查询。"})
			return
		}
		allowFailedRetry = true
	}
	st, raw, err := cli.Preview(c.Request.Context(), code, deviceFrom(c))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	// 成功时记下 code ↔ redemption_token，供后续绑定 session / 账单查卡密
	if st >= 200 && st < 300 {
		tok := extractJSONString(raw, "redemption_token", "token")
		if tok == "" {
			tok = extractJSONNestedString(raw, "data", "redemption_token")
		}
		if tok == "" {
			c.JSON(http.StatusBadGateway, gin.H{"error": "验证服务未返回安全查询凭证，请稍后重试。"})
			return
		}
		var attemptToken string
		var persistErr error
		if allowFailedRetry {
			attemptToken, persistErr = db.BindCDKRedemptionTokenCASForFailedRetryWithAttempt(code, tok, snapshot)
		} else {
			attemptToken, persistErr = db.BindCDKRedemptionTokenCASWithAttempt(code, tok, snapshot)
		}
		if persistErr != nil {
			log.Printf("[cdk-preview] binding persistence failed: %v", persistErr)
			if errors.Is(persistErr, db.ErrCDKBindingChanged) || errors.Is(persistErr, db.ErrCDKQueryAlreadyStarted) {
				c.JSON(http.StatusConflict, gin.H{"error": "卡密状态已变化，请重新验证；本次未提交付款。"})
			} else {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂时无法安全保存兑换进度，本次未提交付款，请稍后重试。"})
			}
			return
		}
		var previewPayload map[string]any
		if json.Unmarshal(raw, &previewPayload) != nil || previewPayload == nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "验证服务返回格式异常，请稍后重试。"})
			return
		}
		previewPayload["attempt_token"] = attemptToken
		raw, err = json.Marshal(previewPayload)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂时无法生成安全兑换凭证，请稍后重试。"})
			return
		}
	}
	proxyPublicJSON(c, st, raw)
}

// PublicCDKPreflight POST /api/v1/public/cdk/preflight
func PublicCDKPreflight(c *gin.Context) {
	privateNoStore(c)
	var body map[string]any
	if !localBody(c, &body) {
		return
	}
	tok := strings.TrimSpace(str(body["redemption_token"]))
	attemptToken := strings.TrimSpace(str(body["attempt_token"]))
	if tok == "" || len(tok) > 512 || attemptToken == "" || len(attemptToken) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "兑换凭证无效，请重新验证卡密；本次未提交付款。"})
		return
	}
	// Validate the immutable preview identity before contacting the upstream
	// service. An old token, a token for another CDK, or an already-submitted
	// redemption must never be able to replace credentials or renew retention.
	binding, err := db.GetCDKPreflightBindingByToken(tok, time.Now())
	if err != nil {
		switch {
		case errors.Is(err, db.ErrCDKPreflightExpired), errors.Is(err, db.ErrCDKQueryExpired):
			c.JSON(http.StatusGone, gin.H{"error": "本次验证已过期，请重新验证卡密；本次未提交付款。"})
		case errors.Is(err, db.ErrCDKQueryAlreadyStarted):
			c.JSON(http.StatusConflict, gin.H{"error": "该卡密已提交兑换，请使用卡密查询查看处理进度。"})
		case errors.Is(err, sql.ErrNoRows):
			c.JSON(http.StatusConflict, gin.H{"error": "兑换凭证已失效，请重新验证卡密；本次未提交付款。"})
		default:
			log.Printf("[cdk-preflight] preview validation failed tok=%s: %v", shortTok(tok), err)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂时无法安全核验兑换凭证，本次未提交付款，请稍后重试。"})
		}
		return
	}
	if binding == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "兑换凭证已失效，请重新验证卡密；本次未提交付款。"})
		return
	}
	if binding.AttemptNonce != attemptToken {
		c.JSON(http.StatusConflict, gin.H{"error": "本次验证已被新的操作替换，请重新验证卡密；本次未提交付款。"})
		return
	}
	requestedCode := normalizePublicCDK(str(body["code"]))
	if requestedCode != "" && (!validPublicCDK(requestedCode) || requestedCode != binding.CDKCode) {
		c.JSON(http.StatusConflict, gin.H{"error": "卡密与兑换凭证不匹配，请重新验证卡密；本次未提交付款。"})
		return
	}
	body["code"] = binding.CDKCode
	body["redemption_token"] = tok
	delete(body, "attempt_token")
	if err := db.CheckCDKPreflightGrantCapacity(attemptToken); err != nil {
		if errors.Is(err, db.ErrCDKPreflightLimit) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "本次验证账号次数过多，请重新验证卡密后再试；本次未提交付款。"})
		} else {
			log.Printf("[cdk-preflight] capacity check failed tok=%s: %v", shortTok(tok), err)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂时无法安全核验预检次数，本次未提交付款，请稍后重试。"})
		}
		return
	}

	cli := cardplatform.NewFromSettings()
	st, raw, err := cli.Preflight(c.Request.Context(), body, deviceFrom(c))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	// Preflight is intentionally not persisted as billing identity. Multiple
	// account checks may race within one attempt; only the account returned by
	// the single claimed redeem/result is authoritative.
	if st >= 200 && st < 300 {
		preflightToken := extractJSONString(raw, "preflight_token")
		if preflightToken == "" {
			preflightToken = extractJSONNestedString(raw, "data", "preflight_token")
		}
		if preflightToken == "" {
			c.JSON(http.StatusBadGateway, gin.H{"error": "预检服务未返回安全提交凭证，请重新预检。"})
			return
		}
		if err := db.RegisterCDKPreflightGrant(binding.CDKCode, tok, attemptToken, preflightToken, time.Now()); err != nil {
			log.Printf("[cdk-preflight] grant persistence failed tok=%s: %v", shortTok(tok), err)
			switch {
			case errors.Is(err, db.ErrCDKPreflightExpired), errors.Is(err, db.ErrCDKQueryExpired):
				c.JSON(http.StatusGone, gin.H{"error": "本次验证已过期，请重新验证卡密；本次未提交付款。"})
			case errors.Is(err, db.ErrCDKBindingMismatch), errors.Is(err, db.ErrCDKBindingChanged),
				errors.Is(err, db.ErrCDKQueryAlreadyStarted), errors.Is(err, sql.ErrNoRows):
				c.JSON(http.StatusConflict, gin.H{"error": "本次验证已被新的操作替换，请重新验证卡密；本次未提交付款。"})
			default:
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂时无法安全保存预检结果，本次未提交付款，请稍后重试。"})
			}
			return
		}
	}
	proxyPublicJSON(c, st, raw)
}

// PublicCDKRedeem POST /api/v1/public/cdk/redeem
func PublicCDKRedeem(c *gin.Context) {
	privateNoStore(c)
	// Frontends may only offer a direct retry when the server explicitly proves
	// this request never crossed the one-way submit latch. Once claimed (or when
	// another request already claimed it), every error becomes read-only polling.
	c.Header("X-Maple-Submit-State", "not-submitted")
	var body map[string]any
	if !localBody(c, &body) {
		return
	}
	// This is the first actual submit action. The atomic DB update only fills
	// an empty window and consumes a one-way latch, so concurrent/replayed
	// requests can never call the upstream redeem endpoint twice.
	tok := strings.TrimSpace(str(body["redemption_token"]))
	attemptToken := strings.TrimSpace(str(body["attempt_token"]))
	preflightToken := strings.TrimSpace(str(body["preflight_token"]))
	if tok == "" || attemptToken == "" || len(attemptToken) > 128 ||
		preflightToken == "" || len(preflightToken) > 2048 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "兑换凭证无效，请重新验证卡密；本次未提交付款。"})
		return
	}
	// Read and validate every server-owned card-safety input before consuming
	// the one-way submit latch. If the local policy/selection/blocklist storage
	// is unavailable, the customer can safely retry and no upstream payment can
	// have been submitted without those protections.
	policySnapshot, err := loadPublicRedeemPolicySnapshot()
	if err != nil {
		log.Printf("[cdk-redeem] card safety snapshot failed tok=%s: %v", shortTok(tok), err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂时无法安全读取选卡保护，本次未提交付款，请稍后重试。"})
		return
	}
	// From this point until Claim returns a classified result, the database
	// outcome is deliberately treated as unknown. A transport/storage error
	// must never invite the browser to submit the same purchase again.
	c.Header("X-Maple-Submit-State", "unknown")
	if err := db.ClaimCDKRedemptionAttempt(tok, attemptToken, preflightToken, time.Now()); err != nil {
		log.Printf("[cdk-redeem] submit claim failed tok=%s: %v", shortTok(tok), err)
		switch {
		case errors.Is(err, db.ErrCDKPreflightExpired):
			c.Header("X-Maple-Submit-State", "not-submitted")
			c.JSON(http.StatusGone, gin.H{"error": "本次验证已超过 24 小时，请重新验证卡密；本次未提交付款。"})
		case errors.Is(err, db.ErrCDKQueryExpired):
			c.Header("X-Maple-Submit-State", "not-submitted")
			c.JSON(http.StatusGone, gin.H{"error": "该卡密的 7 天查询期已结束，不能再次提交兑换。"})
		case errors.Is(err, db.ErrCDKSubmitAlreadyClaimed):
			c.Header("X-Maple-Submit-State", "submitted")
			c.JSON(http.StatusConflict, gin.H{"error": "本次兑换已提交，请使用卡密查询查看处理进度。"})
		case errors.Is(err, db.ErrCDKBindingMismatch), errors.Is(err, db.ErrCDKBindingChanged):
			c.Header("X-Maple-Submit-State", "not-submitted")
			c.JSON(http.StatusConflict, gin.H{"error": "本次验证已被新的操作替换，请重新验证卡密；本次未重复提交。"})
		case errors.Is(err, sql.ErrNoRows):
			c.Header("X-Maple-Submit-State", "not-submitted")
			c.JSON(http.StatusConflict, gin.H{"error": "兑换凭证已失效，请重新验证卡密；本次未提交付款。"})
		default:
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "提交状态暂时无法确认，请勿重复提交；请稍后使用原始卡密查询处理进度。"})
		}
		return
	}
	c.Header("X-Maple-Submit-State", "submitted")
	// The public caller owns no card-selection or idempotency fields. Rebuild an
	// allowlisted upstream request after the claim so malicious policy overrides,
	// arbitrary card IDs and colliding client_request_id values cannot cross the
	// BFF boundary. The deterministic ID is unique to this server-issued attempt.
	requestHash := sha256.Sum256([]byte(attemptToken))
	body = map[string]any{
		"redemption_token":  tok,
		"preflight_token":   preflightToken,
		"client_request_id": "maple-cdk-" + hex.EncodeToString(requestHash[:])[:24],
	}
	// Use only the validated pre-claim snapshot from this point onward. There
	// are deliberately no DB reads between the claim and the upstream submit.
	applyPublicRedeemPolicySnapshot(body, policySnapshot)
	cli := cardplatform.NewFromSettings()
	st, raw, err := cli.Redeem(c.Request.Context(), body, deviceFrom(c))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if st >= 200 && st < 300 {
		var payload map[string]any
		if json.Unmarshal(raw, &payload) == nil && payload != nil {
			if email := extractPublicResultEmail(payload); email != "" {
				if binding, bindErr := db.GetPublicCDKBindingByToken(tok, time.Now()); bindErr == nil &&
					binding != nil && binding.AttemptNonce == attemptToken {
					_ = db.BindCDKAccountEmailForAttempt(binding.CDKCode, tok, attemptToken, email)
				}
			}
		}
	}
	proxyPublicJSON(c, st, raw)
}

func publicCDKBindingError(c *gin.Context, err error) bool {
	switch {
	case errors.Is(err, db.ErrCDKQueryExpired):
		c.JSON(http.StatusGone, gin.H{"error": "7 天查询期已结束；如需售后，请联系客服。", "status": "query_expired"})
		return true
	case errors.Is(err, db.ErrCDKQueryNotStarted):
		c.JSON(http.StatusNotFound, gin.H{"error": "未找到已提交的兑换记录。"})
		return true
	case err != nil:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "查询暂时不可用"})
		return true
	default:
		return false
	}
}

func servePublicCDKResult(c *gin.Context, bind *db.CDKBinding) {
	if bind == nil || strings.TrimSpace(bind.RedemptionToken) == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "未找到已提交的兑换记录。"})
		return
	}
	cli := cardplatform.NewFromSettings()
	st, raw, err := cli.Result(c.Request.Context(), bind.RedemptionToken, deviceFrom(c))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "订单状态暂时不可用，请稍后重试。"})
		return
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil || payload == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "订单状态暂时不可用，请稍后重试。"})
		return
	}
	// Result is a network round trip. Revalidate the exact local attempt before
	// exposing any upstream data, because a provider token may be reused after a
	// failed retry or expiry while this request is in flight.
	current, currentErr := db.GetPublicCDKBindingByCode(bind.CDKCode, time.Now())
	if publicCDKBindingError(c, currentErr) {
		return
	}
	if current == nil || current.CDKCode != bind.CDKCode ||
		current.RedemptionToken != bind.RedemptionToken ||
		current.AttemptNonce != bind.AttemptNonce {
		c.JSON(http.StatusConflict, gin.H{"error": "兑换记录状态已变化，请重新使用原始卡密查询。"})
		return
	}
	bind = current
	if st >= 200 && st < 300 {
		go observeFromPublicResult(context.Background(), payload, bind.CDKCode, bind.RedemptionToken, bind.AttemptNonce)
		if email := extractPublicResultEmail(payload); email != "" {
			_ = db.BindCDKAccountEmailForAttempt(bind.CDKCode, bind.RedemptionToken, bind.AttemptNonce, email)
		}
	}
	safe := safePublicResultPayload(payload)
	if st >= 400 {
		safe["status"] = "failed"
		safe["message"] = publicResultMessage("failed")
	}
	safe["query_expires_at"] = time.Unix(bind.QueryExpiresAt, 0).UTC().Format(time.RFC3339)
	safe["query_window_days"] = 7
	c.JSON(st, safe)
}

// PublicCDKResult POST /api/v1/public/cdk/result
func PublicCDKResult(c *gin.Context) {
	privateNoStore(c)
	var req struct {
		Token           string `json:"token"`
		RedemptionToken string `json:"redemption_token"`
		AttemptToken    string `json:"attempt_token"`
		AttemptNonce    string `json:"attempt_nonce"`
	}
	if !localBody(c, &req) {
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		token = strings.TrimSpace(req.RedemptionToken)
	}
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token required"})
		return
	}
	attemptToken := strings.TrimSpace(req.AttemptToken)
	if attemptToken == "" {
		attemptToken = strings.TrimSpace(req.AttemptNonce)
	}
	if attemptToken == "" || len(attemptToken) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "attempt_token required；旧页面请改用原始卡密查询。"})
		return
	}
	bind, err := db.GetPublicCDKBindingByToken(token, time.Now())
	if publicCDKBindingError(c, err) {
		return
	}
	if bind == nil || bind.AttemptNonce != attemptToken {
		c.JSON(http.StatusNotFound, gin.H{"error": "未找到与本次验证匹配的兑换记录，请改用原始卡密查询。"})
		return
	}
	servePublicCDKResult(c, bind)
}

// PublicCDKResultByCode POST /api/v1/public/cdk/result-by-code
// 用卡密反查本站绑定的 redemption_token，再转发卡台 result（刷新进度 / 任务查询用）
func PublicCDKResultByCode(c *gin.Context) {
	privateNoStore(c)
	var req struct {
		Code    string `json:"code"`
		CDKCode string `json:"cdk_code"`
	}
	if !localBody(c, &req) {
		return
	}
	code := normalizePublicCDK(req.Code)
	if code == "" {
		code = normalizePublicCDK(req.CDKCode)
	}
	if !validPublicCDK(code) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "code required"})
		return
	}
	bind, err := db.GetPublicCDKBindingByCode(code, time.Now())
	if publicCDKBindingError(c, err) {
		return
	}
	servePublicCDKResult(c, bind)
}

func shortTok(tok string) string {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "empty"
	}
	sum := sha256.Sum256([]byte(tok))
	// A stable hash prefix is enough for correlating server logs without ever
	// writing any portion of the private redemption token itself.
	return hex.EncodeToString(sum[:])[:12]
}

// PublicCDKPlans GET /api/v1/public/cdk/plans
const docsDefaultNote = "★这是文档默认兜底价，不是你的账户实时价★：配置卡台 API Key 后才会返回实时值。" +
	"档位是否开放、点数的比索付款价，一律以卡台实时返回为准。"

// docsDefaultRegistry 未配置 API Key / 卡台不可达时的参考价目表。
//
// ★点数必须带上 checkout_amount_minor★：点数的 $0.10 只是我们的服务费，
// 代理真正要垫的是那笔比索付款（₱565/₱1130/₱2260）。只列服务费的话，
// 代理会把「一张 ₱2260 的码」当成一毛钱的东西发出去。
func docsDefaultRegistry() []cardplatform.SellablePlan {
	return []cardplatform.SellablePlan{
		{Key: "plus", Label: "Plus", Flow: "direct", SortOrder: 2, ServiceFeeUsdMinor: 100, ServiceFeeUSD: 1},
		{Key: "pro_5x", Label: "Pro 5x", Flow: "direct", SortOrder: 3, ServiceFeeUsdMinor: 500, ServiceFeeUSD: 5},
		{Key: "pro_20x", Label: "Pro", Flow: "plus_upgrade", SortOrder: 4, ServiceFeeUsdMinor: 1000, ServiceFeeUSD: 10},
		{Key: "credit250", Label: "Codex 点数 250", Flow: "credit", SortOrder: 5, IsCredit: true,
			RequiresActiveSubscription: true, ServiceFeeUsdMinor: 10, ServiceFeeUSD: 0.1,
			CheckoutCurrency: "PHP", CheckoutAmountMinor: 56500},
		{Key: "credit500", Label: "Codex 点数 500", Flow: "credit", SortOrder: 6, IsCredit: true,
			RequiresActiveSubscription: true, ServiceFeeUsdMinor: 10, ServiceFeeUSD: 0.1,
			CheckoutCurrency: "PHP", CheckoutAmountMinor: 113000},
		{Key: "credit1000", Label: "Codex 点数 1000", Flow: "credit", SortOrder: 7, IsCredit: true,
			RequiresActiveSubscription: true, ServiceFeeUsdMinor: 10, ServiceFeeUSD: 0.1,
			CheckoutCurrency: "PHP", CheckoutAmountMinor: 226000},
	}
}

func docsDefaultPlans() map[string]cardplatform.SellablePlan {
	out := map[string]cardplatform.SellablePlan{}
	for _, p := range docsDefaultRegistry() {
		out[p.Key] = p
	}
	return out
}

// 公开展示服务费参考价（不暴露 API Key；若未配置 Key 则返回文档默认价）
func PublicCDKPlans(c *gin.Context) {
	cli := cardplatform.NewFromSettings()
	cfg := cardplatform.LoadConfig()
	if cfg.APIKey == "" {
		c.JSON(http.StatusOK, gin.H{
			"version":  0,
			"source":   "docs_default",
			"plans":    docsDefaultPlans(),
			"registry": docsDefaultRegistry(),
			"note":     docsDefaultNote,
		})
		return
	}
	plans, err := cli.GetPlans(c.Request.Context())
	if err != nil {
		// 降级文档默认
		c.JSON(http.StatusOK, gin.H{
			"version":  0,
			"source":   "docs_default_fallback",
			"error":    err.Error(),
			"plans":    docsDefaultPlans(),
			"registry": docsDefaultRegistry(),
			"note":     docsDefaultNote,
		})
		return
	}
	// 同 CardPlatformPlans：只公开真能买到的档位。
	// 公开页比后台更不能乱列——列了 Claude，客户会照着去问「怎么买不到」。
	sellable := plans.SellablePlans()
	m := map[string]any{}
	for _, p := range sellable {
		m[p.Key] = p
	}
	c.JSON(http.StatusOK, gin.H{
		"version": plans.Version,
		"source":  "cardplatform_live",
		"plans":   m,
		// 档位展示顺序/文案/性质：前端据此渲染，不再维护自己的档位清单
		"registry": sellable,
	})
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(toString(v))
}

func toString(v any) string {
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}

func extractJSONString(raw json.RawMessage, keys ...string) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range keys {
		if s := str(m[k]); s != "" {
			return s
		}
	}
	return ""
}

func extractJSONNestedString(raw json.RawMessage, nest, key string) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	inner, _ := m[nest].(map[string]any)
	if inner == nil {
		return ""
	}
	return str(inner[key])
}

// extractCredentialSession 绑定账单用：优先保留完整 session 材料（sessionToken 或整段 JSON）。
// 纯 accessToken 不再接受（无法 force-refresh）。
func extractCredentialSession(cred any) string {
	m, ok := cred.(map[string]any)
	if !ok {
		return ""
	}
	if s := str(m["session"]); s != "" {
		if looksLikeBareAccessToken(s) {
			return ""
		}
		// JSON 无 sessionToken 则拒
		if strings.HasPrefix(s, "{") {
			var o map[string]any
			if json.Unmarshal([]byte(s), &o) == nil {
				st := str(o["sessionToken"])
				if st == "" {
					st = str(o["session_token"])
				}
				if st == "" {
					return ""
				}
			}
		}
		return s
	}
	// 不再单独接受 accessToken 字段
	return ""
}

func looksLikeBareAccessToken(raw string) bool {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "eyJ") {
		return false
	}
	return len(strings.Split(s, ".")) == 3
}

// CardPlatformSetCDKNote PUT /api/v1/admin/cardplatform/cdks/:id/note
// 本站备注（不写卡台）；空 note 表示清空。
func CardPlatformSetCDKNote(c *gin.Context) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if err := db.SetCardplatformCDKNote(id, req.Note); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	u, _ := c.Get("username")
	username, _ := u.(string)
	action := "cardplatform_cdk_note_set"
	if strings.TrimSpace(req.Note) == "" {
		action = "cardplatform_cdk_note_clear"
	}
	db.WriteAudit(username, action, "id="+strconv.FormatInt(id, 10), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": id, "note": db.GetCardplatformCDKNote(id)})
}

// CardPlatformBatchSetCDKNote POST /api/v1/admin/cardplatform/cdks/batch-note
// body: { ids: number[], note: string }  note 为空则批量清空。
func CardPlatformBatchSetCDKNote(c *gin.Context) {
	var req struct {
		IDs  []int64 `json:"ids"`
		Note string  `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.IDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ids 必填"})
		return
	}
	if len(req.IDs) > 200 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "单次最多 200 条"})
		return
	}
	okN, failed, err := db.BatchSetCardplatformCDKNotes(req.IDs, req.Note)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	u, _ := c.Get("username")
	username, _ := u.(string)
	cleared := strings.TrimSpace(req.Note) == ""
	action := "cardplatform_batch_note_set"
	if cleared {
		action = "cardplatform_batch_note_clear"
	}
	db.WriteAudit(username, action,
		"ok="+strconv.Itoa(okN)+" fail="+strconv.Itoa(len(failed)), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{
		"ok": true, "updated_count": okN, "failed": failed, "failed_count": len(failed),
		"cleared": cleared, "note": strings.TrimSpace(req.Note),
	})
}

// CardPlatformBatchClearCDKNote POST /api/v1/admin/cardplatform/cdks/batch-clear-note
// body: { ids: number[] }
func CardPlatformBatchClearCDKNote(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.IDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ids 必填"})
		return
	}
	if len(req.IDs) > 200 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "单次最多 200 条"})
		return
	}
	n, err := db.ClearCardplatformCDKNotes(req.IDs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	u, _ := c.Get("username")
	username, _ := u.(string)
	db.WriteAudit(username, "cardplatform_batch_note_clear",
		"cleared="+strconv.Itoa(n)+" requested="+strconv.Itoa(len(req.IDs)), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true, "cleared_count": n, "requested": len(req.IDs)})
}
