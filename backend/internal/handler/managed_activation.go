package handler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
	"github.com/tuzi/cdk-recharge-system/internal/jzactivation"
)

const managedFlowName = "managed_activation"

func managedToken(prefix string) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw), nil
}

func managedCodeHash(code string) string       { return localHash(normalizePublicCDK(code)) }
func managedSessionHash(session string) string { return localHash(strings.TrimSpace(session)) }

func managedSessionFromCredential(value any) string {
	credential, ok := value.(map[string]any)
	if !ok || strings.TrimSpace(strAny(credential["mode"])) != "session" {
		return ""
	}
	session := strings.TrimSpace(strAny(credential["session"]))
	if session == "" || !json.Valid([]byte(session)) {
		return ""
	}
	return session
}

func managedErrorMessage(err error, fallback string) string {
	var responseErr *jzactivation.ResponseError
	if errors.As(err, &responseErr) && strings.TrimSpace(responseErr.Message) != "" {
		return responseErr.Message
	}
	return fallback
}

func primaryAllowsManagedFallback(status int, raw []byte) bool {
	if status == http.StatusNotFound {
		return true
	}
	if status != http.StatusBadRequest {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(firstNonEmpty(
		extractJSONString(raw, "error", "message", "msg"),
		extractJSONNestedString(raw, "data", "error"),
	)))
	return strings.Contains(message, "不存在") ||
		strings.Contains(message, "cdk 无效或不可用") ||
		strings.Contains(message, "not found") ||
		strings.Contains(message, "invalid cdk") ||
		strings.Contains(message, "invalid code")
}

// tryManagedPreview is reached only after the primary redemption service has
// definitively rejected the code. Network/server failures on the primary path
// never switch providers, preventing one outage from re-routing valid codes.
func tryManagedPreview(c *gin.Context, code string) bool {
	cfg := jzactivation.LoadConfig()
	if !cfg.Enabled {
		return false
	}
	verified, status, err := jzactivation.New(cfg).Verify(c.Request.Context(), code)
	if err != nil {
		var responseErr *jzactivation.ResponseError
		if errors.As(err, &responseErr) && responseErr.Code == "recharge_closed" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"code": responseErr.Code, "error": managedErrorMessage(err, "兑换通道暂时关闭，请稍后再试")})
			return true
		}
		return false
	}
	if status < 200 || status >= 300 || !verified.Valid {
		if verified.Code == "recharge_closed" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"code": verified.Code, "error": firstNonEmpty(verified.Error, "兑换通道暂时关闭，请稍后再试")})
			return true
		}
		if verified.Code == "cdk_replaced" {
			c.JSON(http.StatusConflict, gin.H{"code": verified.Code, "error": firstNonEmpty(verified.Error, "该卡密已更换，请使用换码时获得的新卡密。")})
			return true
		}
		if verified.Pending {
			c.JSON(http.StatusConflict, gin.H{"status": "processing", "error": firstNonEmpty(verified.Error, "该卡密已有兑换任务正在处理，请使用卡密查询查看进度。")})
			return true
		}
		return false
	}
	attempt, err := managedToken("maple-managed-")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "暂时无法生成安全兑换凭证，请稍后重试。"})
		return true
	}
	err = db.BeginManagedActivation(managedCodeHash(code), localHash(attempt), verified.PlanType, time.Now())
	if errors.Is(err, db.ErrManagedAttemptActive) {
		c.JSON(http.StatusConflict, gin.H{"error": "该卡密已有兑换记录，请使用卡密查询查看进度。"})
		return true
	}
	if err != nil {
		log.Printf("[managed-activation] begin preview: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂时无法安全保存兑换进度，请稍后重试。"})
		return true
	}
	c.JSON(http.StatusOK, gin.H{
		"redemption_token": attempt,
		"attempt_token":    attempt,
		"plan":             verified.PlanType,
		"plan_type":        verified.PlanType,
		"flow":             managedFlowName,
		"credential_modes": []string{"session"},
		"message":          "卡密验证成功，可继续兑换",
	})
	return true
}

func tryManagedPreflight(c *gin.Context, body map[string]any) bool {
	token := strings.TrimSpace(strAny(body["redemption_token"]))
	if token == "" {
		return false
	}
	attempt, err := db.GetManagedActivationByAttemptHash(localHash(token))
	if err != nil || attempt == nil {
		return false
	}
	code := normalizePublicCDK(strAny(body["code"]))
	if !validPublicCDK(code) || managedCodeHash(code) != attempt.CodeHash || attempt.SubmitClaimedAt > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "本次验证已失效或兑换已经提交，请使用卡密查询。"})
		return true
	}
	session := managedSessionFromCredential(body["credential"])
	if session == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请粘贴完整 Session JSON（必须包含 accessToken、sessionToken 和账号邮箱）。"})
		return true
	}
	checked, status, err := jzactivation.NewFromEnv().CheckSubscription(c.Request.Context(), session)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": managedErrorMessage(err, "账号状态暂时无法验证，请稍后重试。")})
		return true
	}
	if status < 200 || status >= 300 || !checked.OK {
		c.JSON(http.StatusBadRequest, gin.H{"code": checked.Code, "error": firstNonEmpty(checked.Error, "账号 Session 无效或已过期，请重新获取。")})
		return true
	}
	preflight, err := managedToken("maple-managed-pf-")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "暂时无法生成安全提交凭证，请稍后重试。"})
		return true
	}
	if err := db.SaveManagedPreflight(attempt.CodeHash, localHash(token), localHash(preflight), managedSessionHash(session), checked.Summary.AccountEmail, time.Now()); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "本次验证状态已经变化，请重新验证卡密。"})
		return true
	}
	c.JSON(http.StatusOK, gin.H{
		"preflight_token":           preflight,
		"flow":                      managedFlowName,
		"email":                     checked.Summary.AccountEmail,
		"account_email":             checked.Summary.AccountEmail,
		"currentPlan":               checked.Summary.PlanType,
		"current_plan":              checked.Summary.PlanType,
		"subscription_has_active":   checked.Summary.HasActiveSubscription,
		"subscription_active_until": checked.Summary.ExpiresAt,
		"subscription_will_renew":   checked.Summary.WillRenew,
	})
	return true
}

func safeManagedRetry(err error) bool {
	var responseErr *jzactivation.ResponseError
	if !errors.As(err, &responseErr) {
		return false
	}
	switch responseErr.HTTPStatus {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return true
	default:
		return false
	}
}

func tryManagedRedeem(c *gin.Context, body map[string]any) bool {
	token := strings.TrimSpace(strAny(body["redemption_token"]))
	if token == "" {
		return false
	}
	attempt, err := db.GetManagedActivationByAttemptHash(localHash(token))
	if err != nil || attempt == nil {
		return false
	}
	c.Header("X-Maple-Submit-State", "not-submitted")
	code := normalizePublicCDK(strAny(body["code"]))
	preflight := strings.TrimSpace(strAny(body["preflight_token"]))
	session := managedSessionFromCredential(body["credential"])
	if !validPublicCDK(code) || managedCodeHash(code) != attempt.CodeHash ||
		preflight == "" || session == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "兑换凭证不完整，请重新验证卡密和 Session；本次未提交。"})
		return true
	}
	c.Header("X-Maple-Submit-State", "unknown")
	err = db.ClaimManagedActivation(attempt.CodeHash, localHash(token), localHash(preflight), managedSessionHash(session), time.Now())
	if err != nil {
		if errors.Is(err, db.ErrManagedSubmitClaimed) {
			c.Header("X-Maple-Submit-State", "submitted")
			c.JSON(http.StatusConflict, gin.H{"error": "本次兑换已经提交，请使用原始卡密查询进度。"})
		} else {
			c.Header("X-Maple-Submit-State", "not-submitted")
			c.JSON(http.StatusConflict, gin.H{"error": "本次验证状态已变化，请重新验证卡密；本次未提交。"})
		}
		return true
	}
	c.Header("X-Maple-Submit-State", "submitted")
	created, status, createErr := jzactivation.NewFromEnv().CreateTask(c.Request.Context(), code, session)
	// Drop the only in-process copy as soon as the provider request returns.
	session = ""
	body["credential"] = nil
	if createErr != nil {
		var responseErr *jzactivation.ResponseError
		if errors.As(createErr, &responseErr) && responseErr.HTTPStatus == http.StatusConflict && responseErr.Code != "cdk_replaced" {
			_ = db.RecordManagedActivationSubmission(attempt.CodeHash, localHash(token), created.TaskID, "submitted", time.Now())
			c.JSON(http.StatusAccepted, managedPendingPayload(attempt.Plan, created.TaskID))
			return true
		}
		if safeManagedRetry(createErr) {
			_ = db.ReleaseManagedActivationClaim(attempt.CodeHash, localHash(token), time.Now())
			c.Header("X-Maple-Submit-State", "not-submitted")
			safeStatus := status
			if safeStatus < 400 || safeStatus > 599 {
				var responseErr *jzactivation.ResponseError
				if errors.As(createErr, &responseErr) {
					safeStatus = responseErr.HTTPStatus
				}
			}
			if safeStatus < 400 || safeStatus > 599 {
				safeStatus = http.StatusServiceUnavailable
			}
			c.JSON(safeStatus, gin.H{"code": responseErrCode(createErr), "error": managedErrorMessage(createErr, "本次未提交，请稍后重新验证。")})
			return true
		}
		_ = db.RecordManagedActivationSubmission(attempt.CodeHash, localHash(token), "", "review", time.Now())
		c.JSON(http.StatusAccepted, gin.H{"status": "review", "message": "提交结果暂时无法确认，请勿重复提交；请使用原始卡密查询。"})
		return true
	}
	if status < 200 || status >= 300 || strings.TrimSpace(created.TaskID) == "" {
		_ = db.RecordManagedActivationSubmission(attempt.CodeHash, localHash(token), created.TaskID, "review", time.Now())
		c.JSON(http.StatusAccepted, gin.H{"status": "review", "message": "提交结果暂时无法确认，请勿重复提交；请使用原始卡密查询。"})
		return true
	}
	_ = db.RecordManagedActivationSubmission(attempt.CodeHash, localHash(token), created.TaskID, "submitted", time.Now())
	c.JSON(http.StatusAccepted, managedPendingPayload(attempt.Plan, created.TaskID))
	return true
}

func responseErrCode(err error) string {
	var responseErr *jzactivation.ResponseError
	if errors.As(err, &responseErr) {
		return responseErr.Code
	}
	return ""
}

func managedPendingPayload(plan, taskID string) gin.H {
	return gin.H{
		"status": "submitted", "stage": "queued", "plan": plan,
		"message": "兑换申请已提交，正在处理。", "task_reference": taskID,
		"provider": "JZ", "provider_label": "JZ 上游", "provider_task_id": taskID,
		"flow": managedFlowName,
	}
}

func managedTaskPayload(task jzactivation.TaskResult, attempt *db.ManagedActivationAttempt) gin.H {
	status := strings.ToLower(strings.TrimSpace(task.TaskStatus))
	safeStatus := "processing"
	stage := "subscription"
	message := "兑换处理中，请稍后查询。"
	canResubmit := false
	switch status {
	case "pending", "submitted":
		safeStatus, stage = status, "queued"
	case "manual_review":
		safeStatus, stage, message = "review", "reconcile", "兑换正在人工处理中，请勿重复提交。"
	case "completed":
		safeStatus, stage, message = "completed", "completed", "兑换已完成"
	case "failed":
		safeStatus, stage, message, canResubmit = "failed", "reconcile", firstNonEmpty(task.FailureReason, "本次兑换未完成，请按提示重新提交或联系客服。"), true
	}
	result := gin.H{
		"status": safeStatus, "stage": stage, "message": message,
		"flow":         managedFlowName,
		"provider": "JZ", "provider_label": "JZ 上游", "provider_task_id": task.TaskID,
		"task_reference": task.TaskID,
		"can_resubmit": canResubmit, "plan": firstNonEmpty(task.PlanType, attempt.Plan),
		"account_email": maskEmail(firstNonEmpty(task.AccountEmail, attempt.AccountEmail)),
		"order": gin.H{
			"status": safeStatus, "stage": stage,
			"plan":          firstNonEmpty(task.PlanType, attempt.Plan),
			"account_email": maskEmail(firstNonEmpty(task.AccountEmail, attempt.AccountEmail)),
			"task_id":       task.TaskID,
			"created_at":    task.CreatedAt, "updated_at": task.UpdatedAt,
			"completed_at": task.CompletedAt, "can_resubmit": canResubmit,
		},
	}
	if task.Challenge != nil && strings.TrimSpace(task.Challenge.ClientSecret) != "" {
		// The challenge is intentionally passed through only to the active
		// customer flow; it is never logged or persisted with the Session.
		result["challenge"] = task.Challenge
		result["order"].(gin.H)["challenge"] = task.Challenge
	}
	if attempt.QueryExpiresAt > 0 {
		result["query_expires_at"] = time.Unix(attempt.QueryExpiresAt, 0).UTC().Format(time.RFC3339)
		result["query_window_days"] = 7
	}
	return result
}

func recordManagedTaskResult(codeHash string, task jzactivation.TaskResult, now time.Time) error {
	activatedAt, expiresAt := int64(0), int64(0)
	if strings.EqualFold(strings.TrimSpace(task.TaskStatus), "completed") {
		completed := parseCompletionTimeAtLocation(task.CompletedAt, now.Unix(), time.FixedZone("UTC+8", 8*3600))
		activatedAt = completed.Unix()
		expiresAt = completed.AddDate(0, 1, 0).Unix()
	}
	return db.RecordManagedActivationResultDetails(codeHash, task.TaskID, task.PlanType, task.TaskStatus,
		task.AccountEmail, task.FailureReason, activatedAt, expiresAt, now)
}

// tryManagedResultByCode returns false only when this code is neither a known
// managed attempt nor a task recognized by the managed activation service.
func tryManagedResultByCode(c *gin.Context, code string) bool {
	codeHash := managedCodeHash(code)
	attempt, dbErr := db.GetManagedActivationByCodeHash(codeHash)
	if dbErr != nil {
		return false
	}
	task, status, err := jzactivation.NewFromEnv().LookupTask(c.Request.Context(), code)
	if err != nil {
		var responseErr *jzactivation.ResponseError
		if errors.As(err, &responseErr) && responseErr.Code == "cdk_replaced" {
			c.JSON(http.StatusConflict, gin.H{"code": responseErr.Code, "error": "该卡密已更换，请使用换码时获得的新卡密。"})
			return true
		}
		if status == http.StatusNotFound && attempt == nil {
			return false
		}
		if attempt != nil {
			cached := jzactivation.TaskResult{TaskID: attempt.TaskID, PlanType: attempt.Plan, AccountEmail: attempt.AccountEmail, TaskStatus: attempt.Status, FailureReason: attempt.FailureReason}
			if cached.TaskStatus == "verified" || cached.TaskStatus == "preflight" || cached.TaskStatus == "not_submitted" {
				c.JSON(http.StatusNotFound, gin.H{"error": "未找到已提交的兑换记录。"})
			} else {
				c.JSON(http.StatusOK, managedTaskPayload(cached, attempt))
			}
			return true
		}
		return false
	}
	if status < 200 || status >= 300 || strings.TrimSpace(task.TaskID) == "" {
		return false
	}
	if attempt == nil || attempt.QueryExpiresAt <= 0 {
		createdAt, parseErr := time.ParseInLocation("2006-01-02 15:04:05", task.CreatedAt, time.FixedZone("UTC+8", 8*3600))
		if parseErr != nil || time.Now().After(createdAt.Add(publicCDKQueryWindow)) {
			c.JSON(http.StatusGone, gin.H{
				"status": "query_expired",
				"error":  "该卡密的公开查询期限已结束。",
			})
			return true
		}
	}
	if attempt == nil {
		// A task may have been submitted on the old page. Create a privacy-safe
		// local tracking row without storing the plaintext code or Session.
		attemptToken, tokenErr := managedToken("maple-imported-")
		if tokenErr == nil {
			_ = db.BeginManagedActivation(codeHash, localHash(attemptToken), task.PlanType, time.Now())
			attempt, _ = db.GetManagedActivationByCodeHash(codeHash)
		}
	}
	if attempt == nil {
		attempt = &db.ManagedActivationAttempt{CodeHash: codeHash, Plan: task.PlanType}
	}
	_ = recordManagedTaskResult(codeHash, task, time.Now())
	refreshed, _ := db.GetManagedActivationByCodeHash(codeHash)
	if refreshed != nil {
		attempt = refreshed
	}
	c.JSON(http.StatusOK, managedTaskPayload(task, attempt))
	return true
}

// PublicManagedChallengeResolved forwards only the non-sensitive challenge
// acknowledgement. The payment secret is handled by Stripe.js in the user's
// browser and is never accepted or stored by MaplePass.
func PublicManagedChallengeResolved(c *gin.Context) {
	var body struct {
		Code   string `json:"cdk_code"`
		TaskID string `json:"task_id"`
	}
	if !localBody(c, &body) || strings.TrimSpace(body.Code) == "" || strings.TrimSpace(body.TaskID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少卡密或任务号"})
		return
	}
	result, status, err := jzactivation.NewFromEnv().ResolveChallenge(c.Request.Context(), normalizePublicCDK(body.Code), strings.TrimSpace(body.TaskID))
	if err != nil {
		var responseErr *jzactivation.ResponseError
		if errors.As(err, &responseErr) && responseErr.HTTPStatus >= 400 && responseErr.HTTPStatus <= 599 {
			c.JSON(responseErr.HTTPStatus, gin.H{"code": responseErr.Code, "error": responseErr.Message})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "验证结果暂时无法回传，请稍后重试"})
		return
	}
	if status < 200 || status >= 300 {
		c.JSON(http.StatusBadGateway, gin.H{"error": "验证结果暂时无法回传，请稍后重试"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": firstNonEmpty(result.Status, "resolved")})
}
