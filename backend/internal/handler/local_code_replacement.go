package handler

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/auth"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

var codeReserveMu sync.Mutex

func codeCipher() (cipher.AEAD, error) {
	key := sha256.Sum256([]byte("maple-code-reserve-v1|" + auth.JWTSecret()))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func sealReserve(code, product string) ([]byte, error) {
	a, err := codeCipher()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return a.Seal(nonce, nonce, []byte(code), []byte(product)), nil
}
func openReserve(raw []byte, product string) (string, error) {
	a, err := codeCipher()
	if err != nil {
		return "", err
	}
	if len(raw) < a.NonceSize() {
		return "", fmt.Errorf("invalid reserve")
	}
	plain, err := a.Open(nil, raw[:a.NonceSize()], raw[a.NonceSize():], []byte(product))
	return string(plain), err
}
func fillCodeReserve(tx *sql.Tx, product, plan string) error {
	label := localCodeLabel(plan)
	if label == "" {
		return fmt.Errorf("unsupported reserve plan")
	}
	var count int
	if err := tx.QueryRow("SELECT COUNT(*) FROM local_code_reserve WHERE product_id=?", product).Scan(&count); err != nil {
		return err
	}
	for ; count < 5; count++ {
		suffix, err := GenerateCDKCode(plan)
		if err != nil {
			return err
		}
		code := label + suffix
		raw, err := sealReserve(code, product)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO local_code_reserve(product_id,code_hash,encrypted_code,created_at) VALUES(?,?,?,?)", product, localHash(code), raw, time.Now().Unix()); err != nil {
			return err
		}
	}
	return nil
}
func MaintainCodeReserve() error {
	codeReserveMu.Lock()
	defer codeReserveMu.Unlock()
	tx, err := db.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT id,plan FROM operations_products WHERE enabled=1")
	if err != nil {
		return err
	}
	products := [][2]string{}
	for rows.Next() {
		var p [2]string
		if err = rows.Scan(&p[0], &p[1]); err != nil {
			rows.Close()
			return err
		}
		products = append(products, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range products {
		if err = fillCodeReserve(tx, p[0], p[1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func LocalCodeReserveStatus(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	rows, err := db.DB.Query("SELECT p.id,p.name,COUNT(r.id) FROM operations_products p LEFT JOIN local_code_reserve r ON r.product_id=p.id WHERE p.enabled=1 GROUP BY p.id,p.name ORDER BY p.id")
	if err != nil {
		localError(c, 503, "备用卡密读取失败")
		return
	}
	defer rows.Close()
	list := []gin.H{}
	for rows.Next() {
		var id, name string
		var count int
		if rows.Scan(&id, &name, &count) != nil {
			localError(c, 503, "备用卡密读取失败")
			return
		}
		list = append(list, gin.H{"product_id": id, "name": name, "count": count, "target": 5})
	}
	if rows.Err() != nil {
		localError(c, 503, "备用卡密读取失败")
		return
	}
	c.JSON(200, gin.H{"list": list})
}

// POST keeps full bearer codes out of browser URLs and access logs.
func LocalCDKSearch(c *gin.Context) {
	var req struct {
		Query  string `json:"q"`
		Status string `json:"status"`
		Page   int    `json:"page"`
	}
	if !localBody(c, &req) {
		return
	}
	q := url.Values{}
	q.Set("q", req.Query)
	q.Set("status", req.Status)
	q.Set("page_size", "100")
	if req.Page > 0 {
		q.Set("page", fmt.Sprint(req.Page))
	}
	requestURL := *c.Request.URL
	requestURL.RawQuery = q.Encode()
	c.Request.URL = &requestURL
	OperationsRecordsList(c)
}
func LocalCDKReplace(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var req struct {
		Code      string `json:"code"`
		Request   string `json:"request_id"`
		Confirmed bool   `json:"confirmed"`
	}
	if !localBody(c, &req) {
		return
	}
	req.Code = strings.ToUpper(strings.TrimSpace(req.Code))
	if !req.Confirmed || len(req.Code) < 15 || len(req.Code) > 100 || len(req.Request) < 16 || len(req.Request) > 80 || c.GetHeader("X-Redemption-Device") == "" {
		localError(c, 400, "请提供未使用卡密并确认换码")
		return
	}
	codeReserveMu.Lock()
	defer codeReserveMu.Unlock()
	tx, err := db.DB.Begin()
	if err != nil {
		localError(c, 503, "暂时无法换码，请用原请求重试")
		return
	}
	defer tx.Rollback()
	var oldID, expires, upstream int64
	var status, request, product, plan string
	var enabled bool
	err = tx.QueryRow(`SELECT l.id,l.status,l.expires_at,l.request_id,l.upstream_id,p.id,p.plan,p.enabled FROM local_cdks l
 LEFT JOIN operations_product_bindings b ON b.local_id=l.id JOIN operations_products p ON p.id=COALESCE(b.product_id,'plus') WHERE l.code_hash=?`, localHash(req.Code)).Scan(&oldID, &status, &expires, &request, &upstream, &product, &plan, &enabled)
	if err != nil {
		localError(c, 400, "卡密无效或不符合换码条件")
		return
	}
	var cipherText []byte
	var requestHash, deviceHash string
	var newID int64
	err = tx.QueryRow("SELECT new_id,request_hash,device_hash,encrypted_code FROM local_code_replacements WHERE old_id=?", oldID).Scan(&newID, &requestHash, &deviceHash, &cipherText)
	if err == nil {
		if requestHash != localHash(req.Request) || deviceHash != localDevice(c) {
			localError(c, 409, "该卡密已更换，旧码不可再次换码")
			return
		}
		code, e := openReserve(cipherText, product)
		if e != nil {
			localError(c, 503, "换码结果暂不可读取，请联系商家")
			return
		}
		c.JSON(200, gin.H{"code": code, "expires_at": expires, "product_id": product, "replayed": true})
		return
	}
	if err != sql.ErrNoRows {
		localError(c, 503, "换码查询失败，请重试")
		return
	}
	if status != "unused" || request != "" || upstream != 0 || expires <= time.Now().Unix() || !enabled {
		localError(c, 409, "仅未使用、未提交付款、未过期且商品可用的卡密可以更换")
		return
	}
	var dedicated int
	if err = tx.QueryRow("SELECT COUNT(*) FROM pro_dedicated_orders WHERE local_id=?", oldID).Scan(&dedicated); err != nil || dedicated > 0 {
		localError(c, 409, "已进入付款流程，不能更换")
		return
	}
	if err = fillCodeReserve(tx, product, plan); err != nil {
		localError(c, 503, "备用卡密准备失败，请稍后重试")
		return
	}
	var reserveID int64
	var hash string
	if err = tx.QueryRow("SELECT id,code_hash,encrypted_code FROM local_code_reserve WHERE product_id=? ORDER BY id LIMIT 1", product).Scan(&reserveID, &hash, &cipherText); err != nil {
		localError(c, 503, "备用卡密暂不可用")
		return
	}
	code, err := openReserve(cipherText, product)
	if err != nil || localHash(code) != hash {
		localError(c, 503, "备用卡密校验失败，请联系商家")
		return
	}
	now := time.Now().Unix()
	result, err := tx.Exec("UPDATE local_cdks SET status='disabled',token_hash='',preflight_hash='',message='客户已自助更换，旧卡密失效' WHERE id=? AND status='unused' AND request_id='' AND upstream_id=0", oldID)
	if err != nil {
		localError(c, 409, "卡密状态已变化，请刷新核对")
		return
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		localError(c, 409, "卡密状态已变化，请刷新核对")
		return
	}
	prefix := code[:strings.Index(code, "-")+5]
	result, err = tx.Exec("INSERT INTO local_cdks(code_hash,prefix,plan,expires_at,created_at,batch_id) VALUES(?,?,?,?,?,?)", hash, prefix, plan, expires, now, fmt.Sprintf("replacement-%d", oldID))
	if err != nil {
		localError(c, 503, "换码失败，旧码未失效，请重试")
		return
	}
	newID, err = result.LastInsertId()
	if err != nil || operationsBindIssuedProduct(tx, newID, product) != nil {
		localError(c, 503, "换码绑定失败，请重试")
		return
	}
	if _, err = tx.Exec("INSERT INTO local_code_replacements VALUES(?,?,?,?,?,?,?)", oldID, newID, localHash(req.Request), localDevice(c), product, cipherText, now); err != nil {
		localError(c, 503, "换码记录失败，请重试")
		return
	}
	if _, err = tx.Exec("DELETE FROM local_code_reserve WHERE id=?", reserveID); err != nil {
		localError(c, 503, "备用库存更新失败，请重试")
		return
	}
	if err = fillCodeReserve(tx, product, plan); err != nil {
		localError(c, 503, "补充备用库存失败，未完成换码，请重试")
		return
	}
	if err = tx.Commit(); err != nil {
		localError(c, 503, "换码结果待确认，请保留原请求重试")
		return
	}
	db.WriteAudit("customer", "local_cdk_replace", fmt.Sprintf("old=%d new=%d", oldID, newID), c.ClientIP())
	c.JSON(200, gin.H{"code": code, "expires_at": expires, "product_id": product})
}
