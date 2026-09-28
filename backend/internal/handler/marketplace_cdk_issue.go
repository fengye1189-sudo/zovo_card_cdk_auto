package handler

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

const marketplaceCDKIssuePurpose = "cdk-jit-issue:v1"

type marketplaceCDKIssueRequest struct {
	OrderID   string `json:"orderId"`
	ProductID string `json:"productId"`
	Count     int    `json:"count"`
}

// marketplaceIssuedCode is deterministic only to make a timed-out signed
// request safely replayable. The HMAC secret never leaves either service and
// the database still stores only the irreversible code hash.
func marketplaceIssuedCode(secret, orderID, productID string, index int, plan string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(fmt.Sprintf("marketplace-cdk:v1:%s:%s:%d", orderID, productID, index)))
	sum := mac.Sum(nil)
	letters := make([]byte, 15)
	for i := range letters {
		letters[i] = byte('A' + int(sum[i])%26)
	}
	return localCodeLabel(plan) + string(letters)
}

// MarketplaceLocalCDKIssue issues plan-bound local CDKs only for a signed
// marketplace order UUID. Replaying the exact request returns the same codes;
// changing the product or quantity for that UUID is rejected.
func MarketplaceLocalCDKIssue(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	raw, ok := verifyMarketplaceCompletionRequest(c, marketplaceCDKIssuePurpose)
	if !ok {
		return
	}
	var req marketplaceCDKIssueRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		localError(c, http.StatusBadRequest, "请求格式不正确")
		return
	}
	orderID, validOrder := canonicalMarketplaceOrderID(req.OrderID)
	req.ProductID = strings.TrimSpace(req.ProductID)
	if !validOrder || !operationsProductID.MatchString(req.ProductID) || req.Count < 1 || req.Count > 20 {
		localError(c, http.StatusBadRequest, "请求参数不正确")
		return
	}
	product, err := operationsProductForIssue(req.ProductID)
	if err != nil {
		localError(c, http.StatusConflict, "商品不存在或已停用")
		return
	}
	secret, err := completionBridgeSecret()
	if err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	batchID := "marketplace-" + orderID
	now := time.Now().Unix()
	codes := make([]string, req.Count)
	for i := range codes {
		codes[i] = marketplaceIssuedCode(secret, orderID, req.ProductID, i, product.Plan)
	}

	tx, err := db.DB.Begin()
	if err != nil {
		localError(c, http.StatusServiceUnavailable, "数据库繁忙")
		return
	}
	defer tx.Rollback()
	var existing int
	if err = tx.QueryRow("SELECT COUNT(*) FROM local_cdks WHERE batch_id=?", batchID).Scan(&existing); err != nil {
		localError(c, http.StatusServiceUnavailable, "查询批次失败")
		return
	}
	if existing != 0 && existing != req.Count {
		localError(c, http.StatusConflict, "订单发码参数冲突")
		return
	}
	for _, code := range codes {
		hash := localHash(code)
		if existing > 0 {
			var storedPlan, storedProduct string
			err = tx.QueryRow(`SELECT l.plan,COALESCE(b.product_id,'') FROM local_cdks l
				LEFT JOIN operations_product_bindings b ON b.local_id=l.id
				WHERE l.batch_id=? AND l.code_hash=?`, batchID, hash).Scan(&storedPlan, &storedProduct)
			if err != nil || storedPlan != product.Plan || storedProduct != product.ID {
				localError(c, http.StatusConflict, "订单发码记录不一致")
				return
			}
			continue
		}
		label := localCodeLabel(product.Plan)
		result, insertErr := tx.Exec("INSERT INTO local_cdks(code_hash,prefix,plan,expires_at,created_at,batch_id) VALUES(?,?,?,?,?,?)",
			hash, code[:len(label)+4], product.Plan, now+90*86400, now, batchID)
		if insertErr != nil {
			if errors.Is(insertErr, sql.ErrNoRows) {
				localError(c, http.StatusConflict, "订单发码冲突")
			} else {
				localError(c, http.StatusServiceUnavailable, "发码失败")
			}
			return
		}
		localID, idErr := result.LastInsertId()
		if idErr != nil || operationsBindIssuedProduct(tx, localID, product.ID) != nil {
			localError(c, http.StatusServiceUnavailable, "商品绑定失败")
			return
		}
	}
	if err = tx.Commit(); err != nil {
		localError(c, http.StatusServiceUnavailable, "发码结果待确认，请重试")
		return
	}
	c.JSON(http.StatusOK, gin.H{"codes": codes, "orderId": orderID, "productId": product.ID,
		"plan": product.Plan, "expiresAt": now + 90*86400, "reused": existing > 0})
}
