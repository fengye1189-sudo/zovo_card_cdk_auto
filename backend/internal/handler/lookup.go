package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

// 公开批量查询上限：每条可能回源卡台 Result，避免一次打爆上游。
const lookupBatchMax = 100
const lookupBatchWorkers = 4

type cdkLookupResult struct {
	CDKCode      string  `json:"cdk_code"`
	Status       string  `json:"status"` // unused | used | failed | disabled | expired | processing | query_expired | unknown
	Used         bool    `json:"used"`
	CanResubmit  bool    `json:"can_resubmit"`
	AccountEmail string  `json:"account_email,omitempty"`
	Plan         string  `json:"plan,omitempty"`
	UsedAt       *string `json:"used_at,omitempty"`
	Notes        string  `json:"notes,omitempty"`
	Message      string  `json:"message"`
}

// LookupCDKStatus POST /api/v1/lookup/cdk
// The CDK is intentionally accepted only in a JSON body so it cannot leak
// into access logs, browser history or analytics URLs.
func LookupCDKStatus(c *gin.Context) {
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "请输入卡密"})
		return
	}

	resp := lookupOneCDK(c.Request.Context(), code, deviceFrom(c))
	if resp.Status == "query_expired" {
		c.JSON(http.StatusGone, resp)
		return
	}
	if resp.Status == "unknown" {
		c.JSON(http.StatusNotFound, gin.H{
			"error":   "未找到该卡密记录",
			"status":  "unknown",
			"used":    false,
			"message": "未找到可查询的卡密记录，请检查输入或联系客服。",
		})
		return
	}
	c.JSON(http.StatusOK, resp)
}

// LookupCDKStatusBatch POST /api/v1/lookup/cdk/batch
// body: { "codes": ["SXC-…"], "text": "可选整段粘贴" }
func LookupCDKStatusBatch(c *gin.Context) {
	privateNoStore(c)
	var req struct {
		Codes []string `json:"codes"`
		Text  string   `json:"text"`
	}
	if !localBody(c, &req) {
		return
	}
	codes := normalizeLookupCodes(append(append([]string{}, req.Codes...), splitLookupText(req.Text)...))
	if len(codes) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请输入至少一张卡密"})
		return
	}
	if len(codes) > lookupBatchMax {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "一次最多查询 " + strconv.Itoa(lookupBatchMax) + " 张卡密",
			"max":   lookupBatchMax,
		})
		return
	}

	results := make([]cdkLookupResult, len(codes))
	sem := make(chan struct{}, lookupBatchWorkers)
	var wg sync.WaitGroup
	ctx := c.Request.Context()
	device := deviceFrom(c)
	for i, code := range codes {
		wg.Add(1)
		go func(i int, code string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = lookupOneCDK(ctx, code, device)
		}(i, code)
	}
	wg.Wait()

	c.JSON(http.StatusOK, gin.H{
		"total":   len(results),
		"max":     lookupBatchMax,
		"results": results,
	})
}

func splitLookupText(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return strings.FieldsFunc(raw, func(r rune) bool {
		return unicode.IsSpace(r) || r == ',' || r == ';' || r == '，' || r == '；'
	})
}

func normalizeLookupCodes(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		code := normalizePublicCDK(item)
		if !validPublicCDK(code) {
			continue
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, code)
	}
	return out
}

func applyLookupFailure(resp *cdkLookupResult, notes string, reusable bool) {
	resp.Status = "failed"
	resp.Used = false
	resp.CanResubmit = reusable
	// Internal/upstream notes can contain provider details, credentials or
	// support comments. Public lookup always maps them to controlled copy.
	resp.Notes = ""
	if reusable {
		resp.Message = "使用失败，卡密已重新激活，可以重新提交"
	} else {
		resp.Message = "使用失败，卡密尚未重新激活，请等待处理后再查"
	}
}

func lookupOneCDK(ctx context.Context, code, deviceID string) cdkLookupResult {
	code = normalizePublicCDK(code)
	resp := cdkLookupResult{CDKCode: code, Status: "unknown", Message: "未找到该卡密记录"}
	now := time.Now()

	// Maple/PULS local codes are stored only as SHA-256 hashes. Their public
	// window starts at the first successful reservation, never at issuance.
	if local, lerr := loadLocalPublicCDK(code, now); local != nil {
		if errors.Is(lerr, errLocalCDKQueryExpired) {
			return publicQueryExpiredResult(code)
		}
		if lerr != nil {
			log.Printf("[lookup-cdk] local lookup failed: %v", lerr)
			return resp
		}
		resp.Plan = local.Plan
		if local.SubmittedAt > 0 {
			resp.AccountEmail = maskEmail(local.Email)
		}
		localStatus := strings.ToLower(strings.TrimSpace(local.Status))
		if local.SubmittedAt <= 0 {
			switch localStatus {
			case "consumed", "completed", "success", "reserved", "review",
				"submitted", "running", "queued", "processing", "failed":
				return publicQueryExpiredResult(code)
			}
		}
		switch localStatus {
		case "consumed", "completed", "success":
			resp.Status, resp.Used = "used", true
			resp.Message = "卡密使用成功"
		case "reserved", "review", "submitted", "running", "queued", "processing":
			resp.Status = "processing"
			resp.Message = "卡密兑换处理中"
		case "failed":
			applyLookupFailure(&resp, "", false)
		case "disabled":
			resp.Status = "disabled"
			resp.Message = "卡密已禁用"
		default:
			if local.ExpiresAt > 0 && now.Unix() >= local.ExpiresAt {
				resp.Status = "expired"
				resp.Message = "卡密已过期"
			} else {
				resp.Status = "unused"
				resp.CanResubmit = true
				resp.Message = "卡密未使用，可以提交兑换"
			}
		}
		return resp
	} else if lerr != nil && !errors.Is(lerr, errLocalCDKQueryExpired) {
		log.Printf("[lookup-cdk] local lookup failed: %v", lerr)
	}

	// A binding is authoritative for the current attempt. A fresh pending
	// preview deliberately shadows every historical task for the same reusable
	// CDK; otherwise an old email/result could leak before the new submission.
	bind, berr := db.GetPublicCDKBindingByCode(code, now)
	if errors.Is(berr, db.ErrCDKQueryExpired) {
		return publicQueryExpiredResult(code)
	}
	if errors.Is(berr, db.ErrCDKQueryNotStarted) {
		return cdkLookupResult{
			CDKCode:     code,
			Status:      "unused",
			CanResubmit: true,
			Message:     "卡密已验证，尚未提交本次兑换",
		}
	}
	if berr != nil {
		log.Printf("[lookup-cdk] binding lookup failed: %v", berr)
		return resp
	}
	activeBinding := bind != nil
	redeemOK := false
	reusable := false
	var submittedAt int64
	if activeBinding {
		submittedAt = bind.QueryStartedAt
		// A local inventory row becoming reusable is not proof that the current
		// upstream attempt may be submitted again. Keep the active attempt locked
		// unless its own Result explicitly returns can_resubmit=true together with
		// a terminal failure state.
		reusable = false
		resp.Status = "processing"
		resp.Message = "卡密兑换处理中"
		if strings.TrimSpace(bind.AccountEmail) != "" {
			resp.AccountEmail = maskEmail(bind.AccountEmail)
		} else if strings.TrimSpace(bind.SessionPayload) != "" {
			resp.AccountEmail = maskEmail(extractEmailFromSession(bind.SessionPayload))
		}
	}

	if !activeBinding {
		var planType, keyStatus string
		var usedAt, expiresAt sql.NullTime
		err := db.DB.QueryRow(`
		SELECT COALESCE(plan_type,''), COALESCE(status,''), used_at, expires_at
		FROM cd_keys WHERE upper(trim(code)) = upper(trim(?))
	`, code).Scan(&planType, &keyStatus, &usedAt, &expiresAt)
		if err == nil {
			resp.Plan = planType
			switch strings.ToLower(keyStatus) {
			case "used":
				resp.Status, resp.Used = "used", true
				resp.Message = "卡密使用成功"
				reusable = false
			case "disabled":
				resp.Status, resp.Used = "disabled", false
				resp.Message = "卡密已禁用"
			case "expired":
				resp.Status, resp.Used = "expired", false
				resp.Message = "卡密已过期"
			case "active", "":
				resp.Status, resp.Used = "unused", false
				resp.Message = "卡密未使用"
				reusable = true
			default:
				resp.Status = "unknown"
				resp.Message = "卡密状态暂不可用"
			}
			if expiresAt.Valid && expiresAt.Time.Before(time.Now()) && resp.Status == "unused" {
				resp.Status, resp.Message = "expired", "卡密已过期"
				reusable = false
			}
			if usedAt.Valid {
				submittedAt = usedAt.Time.Unix()
				s := usedAt.Time.Format("2006-01-02 15:04:05")
				resp.UsedAt = &s
			}
		} else if err != sql.ErrNoRows {
			log.Printf("[lookup-cdk] cd_keys: %v", err)
		}
	}

	if !activeBinding {
		var cpStatus, cpPlan string
		err := db.DB.QueryRow(`
		SELECT COALESCE(status,''), COALESCE(plan,'')
		FROM cardplatform_cdk_codes WHERE upper(trim(code)) = upper(trim(?))
		ORDER BY created_at DESC LIMIT 1
	`, code).Scan(&cpStatus, &cpPlan)
		if err == nil {
			if resp.Plan == "" {
				resp.Plan = cpPlan
			}
			st := strings.ToLower(strings.TrimSpace(cpStatus))
			if st == "" {
				st = "unused"
			}
			switch st {
			case "used", "redeemed", "consumed":
				resp.Status, resp.Used = "used", true
				resp.Message = "卡密使用成功"
				reusable = false
			case "disabled":
				if resp.Status != "used" {
					resp.Status, resp.Used = "disabled", false
					resp.Message = "卡密已禁用"
				}
				reusable = false
			case "unused", "active":
				if resp.Status == "unknown" {
					resp.Status, resp.Used = "unused", false
					resp.Message = "卡密未使用"
				}
				if resp.Status == "unused" {
					reusable = true
				}
			}
		}
	}

	// Legacy task rows have no immutable attempt identity. They are consulted
	// only when no current binding exists; an active attempt is derived solely
	// from its server-only binding and the current upstream result token.
	if !activeBinding {
		// Any historical task without a provable creation time makes the public
		// retention boundary unknowable. Fail closed instead of falling back to a
		// current inventory row and incorrectly advertising the CDK as unused.
		var hasUnanchoredTask int
		if err := db.DB.QueryRow(`
			SELECT EXISTS(
				SELECT 1 FROM recharge_tasks
				WHERE upper(trim(cdk_code)) = upper(trim(?))
				  AND strftime('%s',created_at) IS NULL
			)
		`, code).Scan(&hasUnanchoredTask); err != nil {
			log.Printf("[lookup-cdk] legacy task anchor: %v", err)
			return resp
		}
		if hasUnanchoredTask == 1 {
			return publicQueryExpiredResult(code)
		}
		var distinctTaskEmails int
		var uniqueTaskEmail sql.NullString
		if err := db.DB.QueryRow(`
			SELECT COUNT(DISTINCT lower(trim(account_email))),
			       MIN(NULLIF(trim(account_email),''))
			FROM recharge_tasks
			WHERE upper(trim(cdk_code)) = upper(trim(?))
			  AND trim(COALESCE(account_email,'')) <> ''
		`, code).Scan(&distinctTaskEmails, &uniqueTaskEmail); err != nil {
			log.Printf("[lookup-cdk] legacy task identity: %v", err)
			return resp
		}
		if distinctTaskEmails > 1 {
			return cdkLookupResult{
				CDKCode: code,
				Status:  "unknown",
				Message: "无法安全确认该历史兑换记录的归属，请联系客服核对。",
			}
		}
		var taskStatus string
		var accountEmail, taskNotes sql.NullString
		var taskCompleted sql.NullTime
		var firstTaskCreated int64
		err := db.DB.QueryRow(`
			SELECT COALESCE(latest.task_status,''), latest.account_email,
			       latest.completed_at, COALESCE(latest.notes,''),
			       COALESCE((
			         SELECT MIN(CAST(strftime('%s', earliest.created_at) AS INTEGER))
			         FROM recharge_tasks earliest
			         WHERE upper(trim(earliest.cdk_code)) = upper(trim(?))
			           AND strftime('%s', earliest.created_at) IS NOT NULL
			       ),0)
			FROM recharge_tasks latest
			WHERE upper(trim(latest.cdk_code)) = upper(trim(?))
			  AND strftime('%s', latest.created_at) IS NOT NULL
			ORDER BY latest.created_at DESC LIMIT 1
		`, code, code).Scan(
			&taskStatus, &accountEmail, &taskCompleted, &taskNotes, &firstTaskCreated,
		)
		if err == nil {
			if firstTaskCreated > 0 && (submittedAt == 0 || firstTaskCreated < submittedAt) {
				submittedAt = firstTaskCreated
			}
			if submittedAt > 0 && now.Unix() >= submittedAt+int64(publicCDKQueryWindow/time.Second) {
				return publicQueryExpiredResult(code)
			}
			if distinctTaskEmails == 1 && uniqueTaskEmail.Valid {
				resp.AccountEmail = maskEmail(uniqueTaskEmail.String)
			}
			ts := strings.ToLower(taskStatus)
			switch ts {
			case "completed", "success", "done":
				redeemOK = true
				resp.Status, resp.Used = "used", true
				resp.Message = "卡密使用成功"
				reusable = false
				if taskCompleted.Valid {
					s := taskCompleted.Time.Format("2006-01-02 15:04:05")
					resp.UsedAt = &s
				}
			case "pending", "submitted", "running", "queued", "processing":
				if !redeemOK && resp.Status != "used" {
					resp.Status, resp.Used = "processing", false
					resp.Message = "卡密兑换处理中"
				}
			case "failed", "failed_precharge", "declined", "cancelled", "error":
				if !redeemOK && resp.Status != "used" {
					note := ""
					if taskNotes.Valid {
						note = taskNotes.String
					}
					applyLookupFailure(&resp, note, reusable)
				}
			}
		}
	}
	if submittedAt > 0 && now.Unix() >= submittedAt+int64(publicCDKQueryWindow/time.Second) {
		return publicQueryExpiredResult(code)
	}

	if activeBinding {
		if resp.AccountEmail == "" && strings.TrimSpace(bind.AccountEmail) != "" {
			resp.AccountEmail = maskEmail(bind.AccountEmail)
		}
		if resp.AccountEmail == "" && strings.TrimSpace(bind.SessionPayload) != "" {
			if em := extractEmailFromSession(bind.SessionPayload); em != "" {
				resp.AccountEmail = maskEmail(em)
			}
		}
		if tok := strings.TrimSpace(bind.RedemptionToken); tok != "" {
			cli := cardplatform.NewFromSettings()
			st, raw, rerr := cli.Result(ctx, tok, deviceID)
			// Result is a network round trip. Revalidate the exact attempt
			// before returning even previously loaded masked fields, and before
			// parsing any response; the provider token may be reused meanwhile.
			current, currentErr := db.GetPublicCDKBindingByCode(code, time.Now())
			if errors.Is(currentErr, db.ErrCDKQueryExpired) {
				return publicQueryExpiredResult(code)
			}
			if currentErr != nil || current == nil ||
				current.CDKCode != bind.CDKCode ||
				current.RedemptionToken != bind.RedemptionToken ||
				current.AttemptNonce != bind.AttemptNonce {
				return cdkLookupResult{
					CDKCode: code,
					Status:  "unknown",
					Message: "兑换记录状态已变化，请重新查询。",
				}
			}
			bind = current
			if rerr == nil && st >= 200 && st < 300 && len(raw) > 0 {
				var payload map[string]any
				if json.Unmarshal(raw, &payload) == nil && payload != nil {
					orderStatus := publicResultString(payload, "status")
					email := publicResultString(payload, "account_email")
					if email != "" {
						resp.AccountEmail = maskEmail(email)
					}
					os := strings.ToLower(orderStatus)
					canRetry, explicitRetry := publicResultBool(payload, "can_resubmit")
					reusable = explicitRetry && canRetry
					switch {
					case os == "completed" || os == "success" || os == "done" || os == "paid":
						redeemOK = true
						resp.Status, resp.Used = "used", true
						resp.CanResubmit = false
						resp.Message = "卡密使用成功"
					case os == "failed" || os == "failed_precharge" || os == "declined" || os == "cancelled" || os == "error":
						if !redeemOK {
							applyLookupFailure(&resp, "", reusable)
						}
					case os != "":
						if !redeemOK && resp.Status != "used" && resp.Status != "failed" {
							resp.Status, resp.Used = "processing", false
							resp.Message = "卡密兑换处理中"
						}
					}
				}
			}
		}
	}

	if resp.AccountEmail != "" && resp.Status == "unused" && !resp.CanResubmit {
		resp.Status, resp.Used = "used", true
		resp.Message = "卡密使用成功"
	}
	if resp.Status == "used" {
		resp.Used = true
		resp.CanResubmit = false
		if resp.Message == "" || resp.Message == "未找到该卡密记录" {
			resp.Message = "卡密使用成功"
		}
	}
	if submittedAt <= 0 && !activeBinding {
		switch resp.Status {
		case "used", "processing", "failed":
			// Submitted-state details need a provable immutable start time or an
			// active binding. Legacy/incomplete rows must never become an
			// unlimited public status oracle.
			return publicQueryExpiredResult(code)
		}
	}
	if resp.Status == "unknown" {
		resp.Message = "未找到可查询的卡密记录，请检查输入或联系客服。"
	}
	return resp
}
