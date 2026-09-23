package handler

import (
	"database/sql"
	"strings"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func localUpgradeType(plan string) string {
	switch plan {
	case "pro_5x":
		return "ChatGPT Pro 5X（菲律宾卡升级）"
	case "pro_20x":
		return "ChatGPT Pro 20X"
	case "plus":
		return "ChatGPT Plus"
	case "go":
		return "ChatGPT Go"
	default:
		return "账号升级"
	}
}

func parseCompletionTime(raw string, fallback int64) time.Time {
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC()
		}
	}
	return time.Unix(fallback, 0).UTC()
}

// The upstream reports the authoritative completion timestamp but currently
// does not return the subscription's exact active-until field. Store one
// calendar month as an explicitly estimated expiry so the customer receives a
// useful date without presenting an invented value as authoritative.
func recordLocalCompletionDetails(localID int64, plan, completedAt string, fallback int64) error {
	return recordLocalCompletionDetailsTx(db.DB, localID, plan, completedAt, fallback)
}

type completionDetailsExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func recordLocalCompletionDetailsTx(exec completionDetailsExecutor, localID int64, plan, completedAt string, fallback int64) error {
	activated := parseCompletionTime(completedAt, fallback)
	expires := activated.AddDate(0, 1, 0)
	_, err := exec.Exec(`UPDATE local_cdks SET activated_at=?,subscription_expires_at=?,
		upgrade_type=?,expiry_estimated=1 WHERE id=? AND status='consumed'`,
		activated.Unix(), expires.Unix(), localUpgradeType(plan), localID)
	return err
}
