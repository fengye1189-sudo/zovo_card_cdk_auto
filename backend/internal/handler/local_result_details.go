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
	case "pro_5x_cl":
		return "ChatGPT Pro 5X（智利区充值）"
	case "pro_20x":
		return "ChatGPT Pro 20X"
	case "plus":
		return "ChatGPT Plus"
	case "go":
		return "ChatGPT Go"
	case "credit250":
		return "Codex 点数 250"
	case "credit500":
		return "Codex 点数 500"
	case "credit1000":
		return "Codex 点数 1000"
	case "credit2500":
		return "Codex 点数 2500"
	case "credit5000":
		return "Codex 点数 5000"
	case "credit25000":
		return "Codex 点数 25000"
	default:
		return "账号升级"
	}
}

func parseCompletionTime(raw string, fallback int64) time.Time {
	return parseCompletionTimeAtLocation(raw, fallback, time.UTC)
}

func parseCompletionTimeAtLocation(raw string, fallback int64, location *time.Location) time.Time {
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC()
		}
	}
	if parsed, err := time.ParseInLocation("2006-01-02 15:04:05", raw, location); err == nil {
		return parsed.UTC()
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
	if strings.HasPrefix(plan, "credit") {
		_, err := exec.Exec(`UPDATE local_cdks SET activated_at=?,subscription_expires_at=0,
			upgrade_type=?,expiry_estimated=0 WHERE id=? AND status='consumed'`,
			activated.Unix(), localUpgradeType(plan), localID)
		return err
	}
	expires := activated.AddDate(0, 1, 0)
	_, err := exec.Exec(`UPDATE local_cdks SET activated_at=?,subscription_expires_at=?,
		upgrade_type=?,expiry_estimated=1 WHERE id=? AND status='consumed'`,
		activated.Unix(), expires.Unix(), localUpgradeType(plan), localID)
	return err
}
