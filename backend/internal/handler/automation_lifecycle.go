package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

type automationProductScore struct {
	Product  cardplatform.AutomationProduct
	Success  int64
	Decline  int64
	Attempts int64
}

func automationCardType(issuer string) string {
	raw := strings.ToLower(strings.TrimSpace(issuer))
	switch raw {
	case "starlink", "xlink", "xinglian", "星链", "星链卡":
		return "starlink"
	}
	switch cardplatform.CanonicalCardIssuer(raw) {
	case "one":
		return "channel1"
	case "four":
		// The current upstream groups issuer four as 星链卡. Keeping the
		// server-side name independent of its historical channel alias prevents
		// it from being mixed with 渠道1 in performance reporting.
		return "starlink"
	case "":
		return "unknown"
	default:
		return "other:" + cardplatform.CanonicalCardIssuer(raw)
	}
}

func automationCardTypeLabel(cardType string) string {
	switch cardType {
	case "starlink":
		return "星链卡"
	case "channel1":
		return "渠道1"
	case "unknown", "":
		return "待识别"
	default:
		return strings.TrimPrefix(cardType, "other:")
	}
}

func automationProductHead(code, bin, cardType string) string {
	prefix := strings.TrimSpace(cardType)
	if prefix == "" {
		prefix = "unknown"
	}
	if bin = strings.TrimSpace(bin); bin != "" {
		return prefix + "|bin:" + bin
	}
	return prefix + "|product:" + strings.TrimSpace(code)
}

func recordAutomationOutcome(localID, cardID int64, upstreamState string, now int64) error {
	if localID <= 0 || cardID <= 0 {
		return nil
	}
	outcome := ""
	switch upstreamState {
	case "completed":
		outcome = "success"
	case "declined", "failed_precharge":
		outcome = "decline"
	default:
		return nil
	}
	// A later authoritative completion replaces an earlier decline for success
	// rate reporting. The separate decline ledger remains immutable for the
	// one-decline retirement rule.
	_, err := db.DB.Exec(`INSERT INTO automation_card_outcomes(local_id,card_id,outcome,occurred_at)
	 VALUES(?,?,?,?) ON CONFLICT(local_id) DO UPDATE SET
	 card_id=excluded.card_id,
	 outcome=CASE WHEN excluded.outcome='success' THEN 'success' ELSE automation_card_outcomes.outcome END,
	 occurred_at=CASE WHEN excluded.outcome='success' AND automation_card_outcomes.outcome<>'success'
	 THEN excluded.occurred_at ELSE automation_card_outcomes.occurred_at END`, localID, cardID, outcome, now)
	if err == nil {
		scheduleUpstreamCardRemarkSync()
	}
	return err
}

// recordAutomationDecline counts only exact, authoritative card declines. The
// local_id uniqueness makes repeated polling idempotent.
func recordAutomationDecline(localID, cardID int64, upstreamState string, now int64) error {
	if upstreamState != "declined" && upstreamState != "failed_precharge" {
		return nil
	}
	tx, err := db.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT OR IGNORE INTO automation_card_lifecycle(card_id,created_at,updated_at)
	 VALUES(?,?,?)`, cardID, now, now); err != nil {
		return err
	}
	result, err := tx.Exec(`INSERT OR IGNORE INTO automation_card_declines(local_id,card_id,status,recorded_at)
	 VALUES(?,?,?,?)`, localID, cardID, upstreamState, now)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 1 {
		_, err = tx.Exec(`UPDATE automation_card_lifecycle SET decline_count=decline_count+1,
		 retire_state=CASE WHEN decline_count+1>=1 AND retire_state='active' THEN 'queued' ELSE retire_state END,
		 retire_reason=CASE WHEN decline_count+1>=1 AND retire_state='active' THEN 'one_decline' ELSE retire_reason END,
		 updated_at=? WHERE card_id=?`, now, cardID)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func catalogAutomationInventory(inventory []cardplatform.CardChoice, products map[string]cardplatform.AutomationProduct, now int64) error {
	tx, err := db.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, product := range products {
		if strings.TrimSpace(product.Code) == "" {
			continue
		}
		if _, err = tx.Exec(`INSERT INTO automation_product_catalog(product_code,bin,card_type,first_seen,last_seen)
		 VALUES(?,?,?,?,?) ON CONFLICT(product_code) DO UPDATE SET
		 bin=excluded.bin,card_type=excluded.card_type,last_seen=excluded.last_seen`, product.Code, product.Bin, automationCardType(product.Issuer), now, now); err != nil {
			return err
		}
	}
	for _, card := range inventory {
		if card.ID <= 0 {
			continue
		}
		bin := ""
		if product, ok := products[card.Product]; ok {
			bin = product.Bin
		}
		if _, err = tx.Exec(`INSERT INTO automation_card_lifecycle(card_id,product_code,bin,created_at,updated_at)
		 VALUES(?,?,?,?,?) ON CONFLICT(card_id) DO UPDATE SET
		 product_code=CASE WHEN excluded.product_code<>'' THEN excluded.product_code ELSE automation_card_lifecycle.product_code END,
		 bin=CASE WHEN excluded.bin<>'' THEN excluded.bin ELSE automation_card_lifecycle.bin END,
		 updated_at=excluded.updated_at`, card.ID, card.Product, bin, now, now); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("DELETE FROM automation_product_daily"); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO automation_product_daily(day,product_code,bin,successes,declines,attempts,updated_at)
	 SELECT strftime('%Y-%m-%d',o.occurred_at,'unixepoch','+7 hours'),l.product_code,l.bin,
	 SUM(CASE WHEN o.outcome='success' THEN 1 ELSE 0 END),
	 SUM(CASE WHEN o.outcome='decline' THEN 1 ELSE 0 END),COUNT(*),?
	 FROM automation_card_outcomes o JOIN automation_card_lifecycle l ON l.card_id=o.card_id
	 WHERE l.product_code<>'' GROUP BY 1,2,3`, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func rankedAutomationProducts(products []cardplatform.AutomationProduct, amount, now int64) ([]automationProductScore, error) {
	stats := map[string]automationProductScore{}
	rows, err := db.DB.Query(`SELECT d.product_code,d.bin,COALESCE(c.card_type,''),SUM(d.successes),SUM(d.declines),SUM(d.attempts)
	 FROM automation_product_daily d LEFT JOIN automation_product_catalog c ON c.product_code=d.product_code
	 WHERE d.day>=strftime('%Y-%m-%d',?,'unixepoch','+7 hours','-29 days')
	 GROUP BY d.product_code,d.bin,COALESCE(c.card_type,'')`, now)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var code, bin, cardType string
		var score automationProductScore
		if err = rows.Scan(&code, &bin, &cardType, &score.Success, &score.Decline, &score.Attempts); err != nil {
			rows.Close()
			return nil, err
		}
		key := automationProductHead(code, bin, cardType)
		total := stats[key]
		total.Success += score.Success
		total.Decline += score.Decline
		total.Attempts += score.Attempts
		stats[key] = total
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	ranked := make([]automationProductScore, 0, len(products))
	seen := map[string]bool{}
	for _, product := range products {
		if product.Code == "" || seen[product.Code] || (product.Enabled != nil && !*product.Enabled) {
			continue
		}
		if _, ok := productCost(product, amount, true); !ok {
			continue
		}
		seen[product.Code] = true
		// A card head may be renamed to a new product code by the upstream.
		// Share outcome history by BIN so the ranking follows the actual card
		// head; products without a BIN safely fall back to their product code.
		score := stats[automationProductHead(product.Code, product.Bin, automationCardType(product.Issuer))]
		score.Product = product
		ranked = append(ranked, score)
	}
	// Bayesian smoothing avoids treating a one-payment product as conclusively
	// better than a well-tested product: score=(successes+1)/(attempts+2).
	sort.Slice(ranked, func(i, j int) bool {
		left := (ranked[i].Success + 1) * (ranked[j].Attempts + 2)
		right := (ranked[j].Success + 1) * (ranked[i].Attempts + 2)
		if left != right {
			return left > right
		}
		// Prefer the better-supported result when the smoothed rate ties. This
		// keeps an untested product from outranking a product with real wins.
		if ranked[i].Success != ranked[j].Success {
			return ranked[i].Success > ranked[j].Success
		}
		if ranked[i].Attempts != ranked[j].Attempts {
			return ranked[i].Attempts > ranked[j].Attempts
		}
		if ranked[i].Decline != ranked[j].Decline {
			return ranked[i].Decline < ranked[j].Decline
		}
		return ranked[i].Product.Code < ranked[j].Product.Code
	})
	return ranked, nil
}

func selectAutomationProduct(products []cardplatform.AutomationProduct, amount, now int64) (cardplatform.AutomationProduct, string, error) {
	ranked, err := rankedAutomationProducts(products, amount, now)
	if err != nil {
		return cardplatform.AutomationProduct{}, "", err
	}
	if len(ranked) == 0 {
		return cardplatform.AutomationProduct{}, "", fmt.Errorf("no eligible product")
	}
	var priorOpens int64
	if err = db.DB.QueryRow("SELECT COUNT(*) FROM automation_money WHERE action='open'").Scan(&priorOpens); err != nil {
		return cardplatform.AutomationProduct{}, "", err
	}
	// Four openings exploit the best 30-day smoothed success rate; every fifth
	// opening is a controlled exploration. The exploration slot first chooses
	// a different head with the fewest earlier exploration/open selections,
	// then the fewest real payment samples. This gives newly published heads a
	// guaranteed trial without letting them displace the proven head normally.
	if len(ranked) < 2 || (priorOpens+1)%5 != 0 {
		return ranked[0].Product, "best", nil
	}
	selectionCounts := map[string]int64{}
	rows, err := db.DB.Query(`SELECT s.product_code,s.bin,COALESCE(c.card_type,''),COUNT(*)
	 FROM automation_product_selections s LEFT JOIN automation_product_catalog c ON c.product_code=s.product_code
	 GROUP BY s.product_code,s.bin,COALESCE(c.card_type,'')`)
	if err != nil {
		return cardplatform.AutomationProduct{}, "", err
	}
	for rows.Next() {
		var code, bin, cardType string
		var count int64
		if err = rows.Scan(&code, &bin, &cardType, &count); err != nil {
			rows.Close()
			return cardplatform.AutomationProduct{}, "", err
		}
		selectionCounts[automationProductHead(code, bin, cardType)] += count
	}
	if err = rows.Close(); err != nil {
		return cardplatform.AutomationProduct{}, "", err
	}
	bestHead := automationProductHead(ranked[0].Product.Code, ranked[0].Product.Bin, automationCardType(ranked[0].Product.Issuer))
	exploration := make([]automationProductScore, 0, len(ranked)-1)
	for _, score := range ranked[1:] {
		if automationProductHead(score.Product.Code, score.Product.Bin, automationCardType(score.Product.Issuer)) != bestHead {
			exploration = append(exploration, score)
		}
	}
	if len(exploration) == 0 {
		return ranked[0].Product, "best", nil
	}
	typeSamples := map[string]int64{}
	seenHeads := map[string]bool{}
	for _, score := range ranked {
		cardType := automationCardType(score.Product.Issuer)
		head := automationProductHead(score.Product.Code, score.Product.Bin, cardType)
		if seenHeads[head] {
			continue
		}
		seenHeads[head] = true
		samples := score.Attempts
		if selectionCounts[head] > samples {
			samples = selectionCounts[head]
		}
		typeSamples[cardType] += samples
	}
	sort.SliceStable(exploration, func(i, j int) bool {
		iType := automationCardType(exploration[i].Product.Issuer)
		jType := automationCardType(exploration[j].Product.Issuer)
		iHead := automationProductHead(exploration[i].Product.Code, exploration[i].Product.Bin, iType)
		jHead := automationProductHead(exploration[j].Product.Code, exploration[j].Product.Bin, jType)
		iSamples, jSamples := exploration[i].Attempts, exploration[j].Attempts
		if selectionCounts[iHead] > iSamples {
			iSamples = selectionCounts[iHead]
		}
		if selectionCounts[jHead] > jSamples {
			jSamples = selectionCounts[jHead]
		}
		if iSamples != jSamples {
			return iSamples < jSamples
		}
		if typeSamples[iType] != typeSamples[jType] {
			return typeSamples[iType] < typeSamples[jType]
		}
		if exploration[i].Attempts != exploration[j].Attempts {
			return exploration[i].Attempts < exploration[j].Attempts
		}
		if selectionCounts[iHead] != selectionCounts[jHead] {
			return selectionCounts[iHead] < selectionCounts[jHead]
		}
		return false
	})
	return exploration[0].Product, "explore", nil
}

func removeRetiredCardFromSettings(cardID int64) error {
	for attempt := 0; attempt < 3; attempt++ {
		raw, err := db.GetSetting("local_cdk_settings")
		if err != nil {
			return err
		}
		var settings localSettings
		if err = json.Unmarshal([]byte(raw), &settings); err != nil {
			return err
		}
		ids := localCardIDs(settings)
		nextIDs := make([]int64, 0, len(ids))
		for _, id := range ids {
			if id != cardID {
				nextIDs = append(nextIDs, id)
			}
		}
		if len(nextIDs) == len(ids) {
			return nil
		}
		settings.CardID = 0
		settings.CardIDs = nextIDs
		next, _ := json.Marshal(settings)
		result, err := db.DB.Exec("UPDATE site_settings SET value=?,updated_at=CURRENT_TIMESTAMP WHERE key='local_cdk_settings' AND value=?", string(next), raw)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 1 {
			return nil
		}
	}
	return fmt.Errorf("settings changed")
}

func reconcileRetiredCards(inventory []cardplatform.CardChoice, now int64) {
	present := map[int64]bool{}
	for _, card := range inventory {
		present[card.ID] = true
	}
	rows, err := db.DB.Query(`SELECT card_id,retire_state FROM automation_card_lifecycle
	 WHERE retire_state IN ('queued','inflight','unknown')`)
	if err != nil {
		return
	}
	type item struct {
		id    int64
		state string
	}
	items := []item{}
	for rows.Next() {
		var v item
		if rows.Scan(&v.id, &v.state) == nil {
			items = append(items, v)
		}
	}
	rows.Close()
	for _, item := range items {
		if !present[item.id] {
			_, _ = db.DB.Exec(`UPDATE automation_card_lifecycle SET retire_state='closed',closed_at=?,updated_at=?
			 WHERE card_id=? AND retire_state IN ('queued','inflight','unknown')`, now, now, item.id)
			_ = removeRetiredCardFromSettings(item.id)
			autoResolve("retire:" + strconv.FormatInt(item.id, 10))
		} else if item.state == "inflight" {
			_, _ = db.DB.Exec("UPDATE automation_card_lifecycle SET retire_state='unknown',updated_at=? WHERE card_id=? AND retire_state='inflight'", now, item.id)
			autoAlert("retire:"+strconv.FormatInt(item.id, 10), 0, fmt.Sprintf("卡片 #%d 的销卡结果尚未确认；已停止使用该卡，不会自动重复销卡。请在 Zovo 核对。", item.id))
		} else if item.state == "unknown" {
			autoAlert("retire:"+strconv.FormatInt(item.id, 10), 0, fmt.Sprintf("卡片 #%d 的销卡结果尚未确认；已停止使用该卡，不会自动重复销卡。请在 Zovo 核对。", item.id))
		}
	}
}

// processQueuedCardRetirement performs at most one upstream mutation. A true
// result tells the caller not to combine it with a funding/opening mutation in
// the same automation cycle.
func processQueuedCardRetirement(ctx context.Context, cli *cardplatform.Client, inventory []cardplatform.CardChoice, scope string, expectedVersion, now int64) bool {
	var cardID int64
	var reason string
	err := db.DB.QueryRow(`SELECT l.card_id,l.retire_reason FROM automation_card_lifecycle l
	 WHERE l.retire_state='queued'
	 AND NOT EXISTS(SELECT 1 FROM local_cdks c WHERE c.card_id=l.card_id AND c.status IN ('reserved','review')
	  AND NOT EXISTS(SELECT 1 FROM automation_card_declines d WHERE d.local_id=c.id))
	 AND NOT EXISTS(SELECT 1 FROM automation_money m WHERE (m.card_id=l.card_id OR m.result_card_id=l.card_id)
	  AND m.state IN ('inflight','pending','unknown'))
	 ORDER BY l.updated_at,l.card_id LIMIT 1`).Scan(&cardID, &reason)
	if err != nil {
		return false
	}
	var card *cardplatform.CardChoice
	for i := range inventory {
		if inventory[i].ID == cardID {
			card = &inventory[i]
			break
		}
	}
	if card == nil {
		reconcileRetiredCards(inventory, now)
		return false
	}
	current, version, err := readAutomationPolicy()
	if err != nil || version != expectedVersion || current.Paused || !current.Sync || !current.Retire {
		return false
	}
	currentConfig := cardplatform.LoadConfig()
	if localHash(currentConfig.SiteBase+"|"+currentConfig.APIKey) != scope {
		return false
	}
	before, _ := usdMinor(card.Balance)
	result, err := db.DB.Exec(`UPDATE automation_card_lifecycle SET retire_state='inflight',
	 retire_attempts=retire_attempts+1,before_minor=?,updated_at=?
	 WHERE card_id=? AND retire_state='queued'`, before, now, cardID)
	if err != nil {
		return false
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return false
	}
	key := localHash(scope + "|retire|" + strconv.FormatInt(cardID, 10) + "|" + reason)
	err = cli.AutomationDeleteCard(ctx, cardID, key)
	if err != nil {
		_, _ = db.DB.Exec("UPDATE automation_card_lifecycle SET retire_state='unknown',updated_at=? WHERE card_id=? AND retire_state='inflight'", time.Now().Unix(), cardID)
		autoAlert("retire:"+strconv.FormatInt(cardID, 10), 0, fmt.Sprintf("卡片 #%d 的销卡请求结果不明；已停止使用该卡，不会自动重复销卡。请在 Zovo 核对余额是否退回。", cardID))
		db.WriteAudit("automation", "retire_card", fmt.Sprintf("card=%d reason=%s result=unknown", cardID, reason), "")
		return true
	}
	closedAt := time.Now().Unix()
	_, _ = db.DB.Exec("UPDATE automation_card_lifecycle SET retire_state='closed',closed_at=?,updated_at=? WHERE card_id=? AND retire_state='inflight'", closedAt, closedAt, cardID)
	if err = removeRetiredCardFromSettings(cardID); err != nil {
		autoAlert("retire:"+strconv.FormatInt(cardID, 10), 0, fmt.Sprintf("卡片 #%d 已销卡并退回余额，但未能从本站支付名单移除；该卡已被系统禁用，请刷新设置。", cardID))
	} else {
		autoResolve("retire:" + strconv.FormatInt(cardID, 10))
	}
	db.WriteAudit("automation", "retire_card", fmt.Sprintf("card=%d reason=%s before_minor=%d result=closed", cardID, reason, before), "")
	return true
}

func queueFinalUseRetirement(tx *sql.Tx, cardID, now int64) error {
	_, err := tx.Exec(`UPDATE automation_card_lifecycle SET retire_state='queued',
	 retire_reason='final_two_successes',updated_at=?
	 WHERE card_id=? AND phase='final' AND retire_state='active'`, now, cardID)
	return err
}
