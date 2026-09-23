package handler

import (
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

const publicCDKQueryWindow = 7 * 24 * time.Hour

// All local public-query paths share the earliest positive submission anchor.
// Source priority must never choose a later timestamp and renew public access.
const localSubmissionAnchorSQL = `COALESCE(NULLIF(MIN(
	COALESCE((SELECT MIN(selected_at) FROM local_card_selections WHERE local_id=c.id AND selected_at>0), 9223372036854775807),
	COALESCE((SELECT MIN(created_at) FROM pro_dedicated_orders WHERE local_id=c.id AND created_at>0), 9223372036854775807),
	COALESCE((SELECT MIN(first_seen) FROM automation_watch WHERE local_id=c.id AND first_seen>0), 9223372036854775807)
), 9223372036854775807), 0)`

var (
	errLocalCDKQueryExpired = errors.New("local CDK public query window expired")
	publicCDKPattern        = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]{3,159}$`)
)

// localPublicCDK is the privacy-safe lookup metadata for a locally issued
// code. The code itself remains hash-only in SQLite.
type localPublicCDK struct {
	ID             int64
	Plan           string
	Status         string
	ExpiresAt      int64
	Email          string
	Message        string
	SubmittedAt    int64
	QueryExpiresAt int64
}

func localQueryWindowByID(localID int64, now time.Time) (int64, int64, error) {
	if db.DB == nil {
		return 0, 0, sql.ErrConnDone
	}
	var submittedAt int64
	err := db.DB.QueryRow(`
		SELECT `+localSubmissionAnchorSQL+`
		FROM local_cdks c
		WHERE c.id=?
		LIMIT 1
	`, localID).Scan(&submittedAt)
	if err != nil {
		return 0, 0, err
	}
	if submittedAt <= 0 {
		return 0, 0, nil
	}
	expiresAt := submittedAt + int64(publicCDKQueryWindow/time.Second)
	if now.Unix() >= expiresAt {
		return submittedAt, expiresAt, errLocalCDKQueryExpired
	}
	return submittedAt, expiresAt, nil
}

func normalizePublicCDK(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

func validPublicCDK(code string) bool {
	return publicCDKPattern.MatchString(normalizePublicCDK(code))
}

func privateNoStore(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store, max-age=0")
	c.Header("Pragma", "no-cache")
	c.Header("Expires", "0")
}

// loadLocalPublicCDK derives the immutable public-query window from the first
// successful local reservation. Preview, preflight, polling and last_checked
// are deliberately not considered, so they can never extend the seven days.
func loadLocalPublicCDK(code string, now time.Time) (*localPublicCDK, error) {
	if db.DB == nil {
		return nil, sql.ErrConnDone
	}
	normalized := normalizePublicCDK(code)
	if normalized == "" {
		return nil, nil
	}
	var row localPublicCDK
	err := db.DB.QueryRow(`
		SELECT c.id, COALESCE(c.plan,''), COALESCE(c.status,''),
		       COALESCE(c.expires_at,0), COALESCE(c.email,''),
		       COALESCE(c.message,''),
		       `+localSubmissionAnchorSQL+`
		FROM local_cdks c
		WHERE c.code_hash=?
		LIMIT 1
	`, localHash(normalized)).Scan(
		&row.ID, &row.Plan, &row.Status, &row.ExpiresAt, &row.Email,
		&row.Message, &row.SubmittedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if row.SubmittedAt > 0 {
		row.QueryExpiresAt = row.SubmittedAt + int64(publicCDKQueryWindow/time.Second)
		if now.Unix() >= row.QueryExpiresAt {
			// Keep the private operations record intact for support and ongoing
			// automation; the public handlers redact it after this point.
			row.Email = ""
			return &row, errLocalCDKQueryExpired
		}
	}
	return &row, nil
}

func maskEmail(email string) string {
	email = strings.TrimSpace(email)
	parts := strings.Split(email, "@")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	local := []rune(parts[0])
	visible := string(local[0])
	if len(local) > 2 {
		visible += strings.Repeat("*", len(local)-2) + string(local[len(local)-1])
	} else {
		visible += "*"
	}
	return visible + "@" + parts[1]
}

func publicQueryExpiredResult(code string) cdkLookupResult {
	return cdkLookupResult{
		CDKCode: code,
		Status:  "query_expired",
		Message: "7 天查询期已结束；如需售后，请联系客服并提供原始卡密。",
	}
}
