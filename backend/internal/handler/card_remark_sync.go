package handler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

const automaticCardRemarkPrefix = "【枫叶自动】"

type cardUsageSummary struct {
	Counts    map[string]int
	Declines  int
	LastPlan  string
	LastEvent int64
}

func scheduleUpstreamCardRemarkSync() {
	_, _ = db.DB.Exec("UPDATE automation_card_remark_runtime SET next_run=0 WHERE id=1")
}

func startUpstreamCardRemarkSync(ctx context.Context) {
	go func() {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				remarkCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
				syncUpstreamCardRemarks(remarkCtx)
				cancel()
				timer.Reset(30 * time.Second)
			}
		}
	}()
}

func cardUsageSummaries() (map[int64]*cardUsageSummary, error) {
	summaries := map[int64]*cardUsageSummary{}
	rows, err := db.DB.Query(`SELECT card_id,plan,COUNT(*),MAX(
	 CASE WHEN activated_at>0 THEN activated_at
	      WHEN last_checked>0 THEN last_checked ELSE created_at END)
	 FROM local_cdks WHERE card_id>0 AND status='consumed'
	 GROUP BY card_id,plan`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var cardID, last int64
		var plan string
		var count int
		if err = rows.Scan(&cardID, &plan, &count, &last); err != nil {
			rows.Close()
			return nil, err
		}
		summary := summaries[cardID]
		if summary == nil {
			summary = &cardUsageSummary{Counts: map[string]int{}}
			summaries[cardID] = summary
		}
		summary.Counts[plan] += count
		if last >= summary.LastEvent {
			summary.LastEvent = last
			summary.LastPlan = plan
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}

	rows, err = db.DB.Query(`SELECT card_id,COUNT(*),MAX(recorded_at)
	 FROM automation_card_declines WHERE card_id>0 GROUP BY card_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cardID, last int64
		var count int
		if err = rows.Scan(&cardID, &count, &last); err != nil {
			return nil, err
		}
		summary := summaries[cardID]
		if summary == nil {
			summary = &cardUsageSummary{Counts: map[string]int{}}
			summaries[cardID] = summary
		}
		summary.Declines = count
		if last > summary.LastEvent {
			summary.LastEvent = last
		}
	}
	return summaries, rows.Err()
}

func cardRemarkPlanLabel(plan string) string {
	switch plan {
	case "plus":
		return "Plus"
	case "go":
		return "Go"
	case "pro_5x":
		return "Pro 5X"
	case "pro_20x":
		return "Pro 20X"
	default:
		return strings.TrimSpace(plan)
	}
}

func automaticCardRemark(summary *cardUsageSummary) string {
	if summary == nil {
		summary = &cardUsageSummary{Counts: map[string]int{}}
	}
	total := 0
	parts := make([]string, 0, 4)
	for _, plan := range []string{"plus", "go", "pro_5x", "pro_20x"} {
		count := summary.Counts[plan]
		if count <= 0 {
			continue
		}
		total += count
		parts = append(parts, fmt.Sprintf("%s×%d", cardRemarkPlanLabel(plan), count))
	}
	otherPlans := make([]string, 0)
	for plan, count := range summary.Counts {
		if count > 0 && !supportedLocalPlan(plan) {
			otherPlans = append(otherPlans, plan)
		}
	}
	sort.Strings(otherPlans)
	for _, plan := range otherPlans {
		count := summary.Counts[plan]
		total += count
		parts = append(parts, fmt.Sprintf("%s×%d", cardRemarkPlanLabel(plan), count))
	}
	text := fmt.Sprintf("%s成功%d次", automaticCardRemarkPrefix, total)
	if len(parts) > 0 {
		text += "：" + strings.Join(parts, "，")
	}
	text += fmt.Sprintf("；拒付%d次", summary.Declines)
	if summary.LastEvent > 0 {
		text += "；最近记录 " + time.Unix(summary.LastEvent, 0).In(operationsBangkok).Format("01-02 15:04")
		if label := cardRemarkPlanLabel(summary.LastPlan); label != "" {
			text += " " + label
		}
	}
	return text
}

func mergeUpstreamCardRemark(existing, automatic string) (string, error) {
	manual := existing
	if marker := strings.Index(manual, automaticCardRemarkPrefix); marker >= 0 {
		manual = manual[:marker]
	}
	manual = strings.NewReplacer("\r\n", " | ", "\n", " | ", "\r", " | ").Replace(manual)
	// The upstream currently rejects Other_Symbol characters (for example the
	// legacy ❌ decoration) and line breaks. Remove only those unsupported
	// decorations while retaining the administrator's actual words and digits.
	manual = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.So, r) {
			return -1
		}
		return r
	}, manual)
	manual = strings.Trim(strings.TrimSpace(manual), "| ")
	merged := automatic
	if manual != "" {
		merged = manual + " | " + automatic
	}
	if len([]rune(merged)) > cardplatform.MaxCardRemarkRunes {
		return "", fmt.Errorf("manual card remark leaves no room for automation summary")
	}
	return merged, nil
}

func cardRemarkInventory(ctx context.Context, cli *cardplatform.Client) ([]cardplatform.CardChoice, error) {
	cards := make([]cardplatform.CardChoice, 0)
	seen := map[int64]bool{}
	for page := 1; page <= 5; page++ {
		list, total, err := cli.CardChoices(ctx, page)
		if err != nil {
			return nil, err
		}
		if total > 100 {
			return nil, fmt.Errorf("card remark inventory exceeds 100 cards")
		}
		for _, card := range list {
			if seen[card.ID] {
				return nil, fmt.Errorf("duplicate card in remark inventory")
			}
			seen[card.ID] = true
			cards = append(cards, card)
		}
		if page*20 >= total {
			if len(cards) != total {
				return nil, fmt.Errorf("incomplete card remark inventory")
			}
			return cards, nil
		}
	}
	return nil, fmt.Errorf("card remark inventory did not finish")
}

func syncUpstreamCardRemarks(ctx context.Context) {
	now := time.Now().Unix()
	claim, err := db.DB.Exec(`UPDATE automation_card_remark_runtime SET next_run=?
	 WHERE id=1 AND next_run<=?`, now+300, now)
	if err != nil {
		return
	}
	claimed, _ := claim.RowsAffected()
	if claimed != 1 {
		return
	}

	cli := cardplatform.NewFromSettings()
	cards, err := cardRemarkInventory(ctx, cli)
	if err != nil {
		_, _ = db.DB.Exec("UPDATE automation_card_remark_runtime SET next_run=?,last_error=? WHERE id=1", now+300, "card list unavailable")
		autoAlert("card_remarks", 0, "无法读取上游卡片列表，卡片用途备注本轮未同步；5 分钟后自动重试。")
		return
	}
	summaries, err := cardUsageSummaries()
	if err != nil {
		_, _ = db.DB.Exec("UPDATE automation_card_remark_runtime SET next_run=?,last_error=? WHERE id=1", now+300, "local usage unavailable")
		autoAlert("card_remarks", 0, "无法汇总本站卡片使用记录，未改动上游备注；5 分钟后自动重试。")
		return
	}

	failed := 0
	for _, card := range cards {
		desired, mergeErr := mergeUpstreamCardRemark(card.Remark, automaticCardRemark(summaries[card.ID]))
		if mergeErr != nil {
			failed++
			continue
		}
		if strings.TrimSpace(card.Remark) == desired {
			continue
		}
		if err = cli.SetCardRemark(ctx, card.ID, desired); err != nil {
			failed++
		}
	}
	if failed > 0 {
		_, _ = db.DB.Exec("UPDATE automation_card_remark_runtime SET next_run=?,last_error=? WHERE id=1", now+300, fmt.Sprintf("%d card remarks pending", failed))
		autoAlert("card_remarks", 0, fmt.Sprintf("有 %d 张卡片的用途备注未能同步到上游；未覆盖手工备注，5 分钟后自动重试。", failed))
		return
	}
	_, _ = db.DB.Exec("UPDATE automation_card_remark_runtime SET next_run=?,last_success=?,last_error='' WHERE id=1", now+300, now)
	autoResolve("card_remarks")
}
