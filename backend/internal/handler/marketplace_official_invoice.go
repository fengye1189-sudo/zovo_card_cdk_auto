package handler

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

// marketplaceOfficialInvoicePurpose is deliberately independent from both the
// code-binding and completion-notification signing domains. A valid completion
// callback can therefore never be replayed as an invoice lookup.
const marketplaceOfficialInvoicePurpose = "cdk-official-invoice:v1"

const (
	// The issuer normally creates the invoice shortly after the provider marks
	// the redemption complete. These bounds match the conservative public
	// billing association logic and prevent a later account purchase from being
	// attached to this marketplace order.
	marketplaceInvoiceBeforeCompletion = 15 * time.Minute
	marketplaceInvoiceAfterCompletion  = 2 * time.Hour
)

// marketplaceOfficialInvoiceRequest is a server-to-server request only. It
// deliberately excludes a purchaser identity, plaintext CDK, browser session,
// Telegram ID, and email. The three identifiers must all agree with the
// immutable marketplace binding and authoritative completion outbox row.
type marketplaceOfficialInvoiceRequest struct {
	OrderID  string `json:"orderId"`
	CodeHash string `json:"codeHash"`
	LocalID  int64  `json:"localId"`
}

// marketplaceOfficialInvoiceResponse is safe for the authenticated MaplePass
// server. It is never sent by any public CDK endpoint. MaplePass is responsible
// for checking the receiving order owner again before exposing or redirecting
// to either issuer URL.
type marketplaceOfficialInvoiceResponse struct {
	Status           string `json:"status"`
	HostedInvoiceURL string `json:"hostedInvoiceUrl,omitempty"`
	InvoicePDFURL    string `json:"invoicePdfUrl,omitempty"`
	InvoiceNumber    string `json:"invoiceNumber,omitempty"`
}

type marketplaceOfficialInvoiceAuthority struct {
	Email       string
	CompletedAt int64
}

type marketplaceOfficialInvoiceCandidate struct {
	Number           string
	HostedInvoiceURL string
	InvoicePDFURL    string
}

// MarketplaceOfficialInvoice returns an upstream OpenAI/Stripe invoice only
// to the HMAC-authenticated MaplePass server. A READY response means exactly
// one paid issuer invoice was found near the authoritative completion time.
// PENDING means no paid issuer invoice is available yet (or its private lookup
// service is temporarily unavailable). UNAVAILABLE means the order binding is
// not authoritative, the account cannot be associated safely, the candidates
// are ambiguous, or no trusted issuer URL is present.
func MarketplaceOfficialInvoice(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store, max-age=0")
	c.Header("Pragma", "no-cache")
	c.Header("Expires", "0")
	raw, ok := verifyMarketplaceCompletionRequest(c, marketplaceOfficialInvoicePurpose)
	if !ok {
		return
	}

	var req marketplaceOfficialInvoiceRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		localError(c, http.StatusBadRequest, "请求格式不正确")
		return
	}

	orderID, validOrder := canonicalMarketplaceOrderID(req.OrderID)
	codeHash, validHash := canonicalCodeHash(req.CodeHash)
	if !validOrder || !validHash || req.LocalID <= 0 {
		localError(c, http.StatusBadRequest, "请求参数不正确")
		return
	}

	authority, err := marketplaceOfficialInvoiceAuthorityFor(orderID, codeHash, req.LocalID)
	if err != nil {
		// The authenticated marketplace already knows the requested order, but
		// should not learn why a local issuer record could not be authorized.
		c.JSON(http.StatusOK, marketplaceOfficialInvoiceResponse{Status: "UNAVAILABLE"})
		return
	}
	if authority.Email == "" || authority.CompletedAt <= 0 {
		c.JSON(http.StatusOK, marketplaceOfficialInvoiceResponse{Status: "UNAVAILABLE"})
		return
	}

	invoices, lookupErr := queryAccounthubInvoices(authority.Email)
	if lookupErr != nil {
		// Invoice generation and the internal issuer lookup are asynchronous.
		// Do not expose the (possibly email-bearing) underlying error text.
		c.JSON(http.StatusOK, marketplaceOfficialInvoiceResponse{Status: "PENDING"})
		return
	}
	candidate, state := matchMarketplaceOfficialInvoice(invoices.Invoices, authority.CompletedAt)
	if state != "READY" {
		c.JSON(http.StatusOK, marketplaceOfficialInvoiceResponse{Status: state})
		return
	}
	c.JSON(http.StatusOK, marketplaceOfficialInvoiceResponse{
		Status:           "READY",
		HostedInvoiceURL: candidate.HostedInvoiceURL,
		InvoicePDFURL:    candidate.InvoicePDFURL,
		InvoiceNumber:    candidate.Number,
	})
}

// marketplaceOfficialInvoiceAuthorityFor verifies the three opaque request
// identifiers against the same durable outbox row that was created with the
// authoritative consumed transition. The outbox may still be `sending`: the
// marketplace can call this endpoint while acknowledging the completion event.
func marketplaceOfficialInvoiceAuthorityFor(orderID, codeHash string, localID int64) (marketplaceOfficialInvoiceAuthority, error) {
	if db.DB == nil {
		return marketplaceOfficialInvoiceAuthority{}, errors.New("local database unavailable")
	}
	var result marketplaceOfficialInvoiceAuthority
	err := db.DB.QueryRow(`
		SELECT c.email,o.completed_at
		FROM marketplace_completion_outbox o
		JOIN marketplace_local_cdk_bindings b
		  ON b.local_id=o.local_id
		 AND b.code_hash=o.code_hash
		 AND b.marketplace_order_id=o.marketplace_order_id
		JOIN local_cdks c
		  ON c.id=o.local_id
		 AND c.code_hash=o.code_hash
		WHERE o.marketplace_order_id=?
		  AND o.code_hash=?
		  AND o.local_id=?
		  AND c.status='consumed'
		  AND c.activated_at>0
		  AND c.upstream_completion_verified_at>0
		  AND c.upstream_completion_source='cardplatform_direct_order'
		  AND o.completed_at>0
		LIMIT 1`, orderID, codeHash, localID).Scan(&result.Email, &result.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return marketplaceOfficialInvoiceAuthority{}, errors.New("authoritative completion not found")
	}
	if err != nil {
		return marketplaceOfficialInvoiceAuthority{}, err
	}
	result.Email = strings.TrimSpace(result.Email)
	return result, nil
}

// matchMarketplaceOfficialInvoice returns READY only for one paid invoice
// whose creation time is near this authoritative completion. Invoice numbers
// are used as a stable de-duplication key because AccountHub may return the
// same issuer invoice through more than one source.
func matchMarketplaceOfficialInvoice(invoices []map[string]interface{}, completedAt int64) (marketplaceOfficialInvoiceCandidate, string) {
	if completedAt <= 0 {
		return marketplaceOfficialInvoiceCandidate{}, "UNAVAILABLE"
	}
	byNumber := make(map[string]marketplaceOfficialInvoiceCandidate)
	for _, invoice := range invoices {
		createdAt, ok := marketplaceInvoiceCreatedAt(invoice["created"])
		if !ok || createdAt < completedAt-int64(marketplaceInvoiceBeforeCompletion/time.Second) || createdAt > completedAt+int64(marketplaceInvoiceAfterCompletion/time.Second) {
			continue
		}
		if !marketplaceInvoicePaid(invoice) {
			continue
		}
		number, validNumber := publicInvoiceNumber(invoice["number"])
		if !validNumber {
			return marketplaceOfficialInvoiceCandidate{}, "UNAVAILABLE"
		}
		hosted := trustedMarketplaceIssuerURL(marketplaceInvoiceString(invoice["hosted_invoice_url"]))
		pdf := trustedMarketplaceIssuerURL(marketplaceInvoiceString(invoice["invoice_pdf"]))
		if hosted == "" && pdf == "" {
			return marketplaceOfficialInvoiceCandidate{}, "UNAVAILABLE"
		}
		candidate := marketplaceOfficialInvoiceCandidate{Number: number, HostedInvoiceURL: hosted, InvoicePDFURL: pdf}
		if existing, exists := byNumber[number]; exists {
			// The same invoice may appear twice from an issuer response. Keep the
			// richer trusted representation, but fail closed if the supposedly
			// identical invoice carries conflicting bearer URLs.
			if (existing.HostedInvoiceURL != "" && candidate.HostedInvoiceURL != "" && existing.HostedInvoiceURL != candidate.HostedInvoiceURL) ||
				(existing.InvoicePDFURL != "" && candidate.InvoicePDFURL != "" && existing.InvoicePDFURL != candidate.InvoicePDFURL) {
				return marketplaceOfficialInvoiceCandidate{}, "UNAVAILABLE"
			}
			if existing.HostedInvoiceURL == "" {
				existing.HostedInvoiceURL = candidate.HostedInvoiceURL
			}
			if existing.InvoicePDFURL == "" {
				existing.InvoicePDFURL = candidate.InvoicePDFURL
			}
			byNumber[number] = existing
			continue
		}
		byNumber[number] = candidate
	}

	if len(byNumber) == 0 {
		if time.Now().Unix() > completedAt+int64(marketplaceInvoiceAfterCompletion/time.Second) {
			return marketplaceOfficialInvoiceCandidate{}, "UNAVAILABLE"
		}
		return marketplaceOfficialInvoiceCandidate{}, "PENDING"
	}
	if len(byNumber) != 1 {
		return marketplaceOfficialInvoiceCandidate{}, "UNAVAILABLE"
	}
	for _, candidate := range byNumber {
		return candidate, "READY"
	}
	return marketplaceOfficialInvoiceCandidate{}, "UNAVAILABLE"
}

func marketplaceInvoiceCreatedAt(raw interface{}) (int64, bool) {
	switch value := raw.(type) {
	case float64:
		return int64(value), value > 0
	case float32:
		return int64(value), value > 0
	case int64:
		return value, value > 0
	case int:
		return int64(value), value > 0
	case json.Number:
		parsed, err := value.Int64()
		return parsed, err == nil && parsed > 0
	case string:
		trimmed := strings.TrimSpace(value)
		if parsed, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return parsed, parsed > 0
		}
		if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
			return parsed.Unix(), true
		}
	}
	return 0, false
}

func marketplaceInvoicePaid(invoice map[string]interface{}) bool {
	if paid, exists := invoice["paid"].(bool); exists {
		// A provider response that says both paid=false and status=paid is
		// inconsistent. Prefer the explicit boolean and fail closed.
		return paid
	}
	return strings.EqualFold(strings.TrimSpace(marketplaceInvoiceString(invoice["status"])), "paid")
}

func marketplaceInvoiceString(raw interface{}) string {
	value, _ := raw.(string)
	return strings.TrimSpace(value)
}

// publicInvoiceMetadata is intentionally narrower than an issuer invoice
// object. Public routes may show a customer that an invoice exists, but they
// must never return bearer-style hosted/PDF URLs, customer identity, metadata,
// provider-only identifiers, or arbitrary issuer text. The latter could embed
// a URL or customer data in a description. The HMAC-authenticated marketplace
// endpoint is the only route allowed to receive a matched issuer URL.
func publicInvoiceMetadata(invoices []map[string]interface{}) []map[string]interface{} {
	if len(invoices) == 0 {
		return []map[string]interface{}{}
	}
	result := make([]map[string]interface{}, 0, len(invoices))
	for _, invoice := range invoices {
		safe := make(map[string]interface{})
		if value, ok := publicInvoiceNumber(invoice["number"]); ok {
			safe["number"] = value
		}
		if value, ok := publicInvoiceStatus(invoice["status"]); ok {
			safe["status"] = value
		}
		if value, ok := invoice["paid"].(bool); ok {
			safe["paid"] = value
		}
		if value, ok := publicInvoiceCurrency(invoice["currency"]); ok {
			safe["currency"] = value
		}
		if value, ok := publicInvoiceAmount(invoice["total"]); ok {
			safe["total"] = value
		}
		if value, ok := publicInvoiceAmount(invoice["amount_paid"]); ok {
			safe["amount_paid"] = value
		}
		if value, ok := marketplaceInvoiceCreatedAt(invoice["created"]); ok {
			safe["created"] = value
		}
		if len(safe) > 0 {
			result = append(result, safe)
		}
	}
	return result
}

func publicInvoiceNumber(raw interface{}) (string, bool) {
	value, ok := raw.(string)
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 160 || strings.Contains(value, "@") || strings.Contains(strings.ToLower(value), "http") {
		return "", false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-_.:/# ", r)) {
			return "", false
		}
	}
	return value, true
}

func publicInvoiceStatus(raw interface{}) (string, bool) {
	value, ok := raw.(string)
	if !ok {
		return "", false
	}
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "draft", "open", "paid", "uncollectible", "void":
		return value, true
	default:
		return "", false
	}
}

func publicInvoiceCurrency(raw interface{}) (string, bool) {
	value, ok := raw.(string)
	if !ok {
		return "", false
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 3 {
		return "", false
	}
	for _, r := range value {
		if r < 'a' || r > 'z' {
			return "", false
		}
	}
	return value, true
}

func publicInvoiceAmount(raw interface{}) (interface{}, bool) {
	switch value := raw.(type) {
	case float64:
		if value >= 0 && value < 9e15 {
			return value, true
		}
	case float32:
		if value >= 0 && value < 9e15 {
			return value, true
		}
	case int:
		if value >= 0 {
			return value, true
		}
	case int64:
		if value >= 0 {
			return value, true
		}
	case json.Number:
		if value, err := value.Int64(); err == nil && value >= 0 {
			return value, true
		}
	}
	return nil, false
}

// trustedMarketplaceIssuerURL is deliberately stricter than substring
// matching. The issuer endpoint can only hand MaplePass a HTTPS URL on a real
// OpenAI, ChatGPT, or Stripe host; e.g. `notstripe.com` is rejected.
func trustedMarketplaceIssuerURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Host == "" || (parsed.Port() != "" && parsed.Port() != "443") {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	for _, suffix := range []string{"openai.com", "chatgpt.com", "stripe.com"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return parsed.String()
		}
	}
	return ""
}
