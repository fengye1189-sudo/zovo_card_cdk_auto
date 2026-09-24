package handler

import (
	"context"
	"testing"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func TestTwoDistinctEmailDeclinesQueueRetirementOnce(t *testing.T) {
	newLocalFixture(t)
	now := time.Now().Unix()
	if err := recordAutomationDecline(1, 123, "first@example.com", "requires_action", now); err != nil {
		t.Fatal(err)
	}
	if err := recordAutomationDecline(1, 123, "first@example.com", "declined", now); err != nil {
		t.Fatal(err)
	}
	if err := recordAutomationDecline(1, 123, "first@example.com", "declined", now+1); err != nil {
		t.Fatal(err)
	}
	var declines int
	var state, reason string
	if err := db.DB.QueryRow("SELECT decline_count,retire_state,retire_reason FROM automation_card_lifecycle WHERE card_id=123").Scan(&declines, &state, &reason); err != nil {
		t.Fatal(err)
	}
	if declines != 1 || state != "active" || reason != "" {
		t.Fatal("one email incorrectly queued retirement", declines, state, reason)
	}
	if err := recordAutomationDecline(2, 123, " FIRST@example.com ", "failed_precharge", now+2); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow("SELECT decline_count,retire_state,retire_reason FROM automation_card_lifecycle WHERE card_id=123").Scan(&declines, &state, &reason); err != nil {
		t.Fatal(err)
	}
	if declines != 2 || state != "active" || reason != "" {
		t.Fatal("same normalized email incorrectly queued retirement", declines, state, reason)
	}
	if err := recordAutomationDecline(3, 123, "second@example.com", "declined", now+3); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow("SELECT decline_count,retire_state,retire_reason FROM automation_card_lifecycle WHERE card_id=123").Scan(&declines, &state, &reason); err != nil {
		t.Fatal(err)
	}
	if declines != 3 || state != "queued" || reason != "two_distinct_email_declines" {
		t.Fatal("wrong decline retirement state", declines, state, reason)
	}
}

func TestAutomaticRetirementIsSafeAndIdempotent(t *testing.T) {
	for _, mode := range []string{"success", "unknown", "busy"} {
		t.Run(mode, func(t *testing.T) {
			a := newAutoFixture(t)
			p := moneyPolicy()
			p.Topup, p.Open, p.Retire = false, false, true
			putAutoPolicy(t, p)
			now := time.Now().Unix()
			if _, err := db.DB.Exec("INSERT OR REPLACE INTO automation_card_lifecycle(card_id,product_code,bin,phase,decline_count,retire_state,retire_reason,created_at,updated_at) VALUES(123,'TEST','537872','primary',2,'queued','two_distinct_email_declines',?,?)", now, now); err != nil {
				t.Fatal(err)
			}
			if mode == "unknown" {
				a.fail = true
			}
			if mode == "busy" {
				if _, err := db.DB.Exec("INSERT INTO local_cdks(id,code_hash,prefix,plan,status,expires_at,created_at,card_id) VALUES(1,'busy','x','plus','review',0,0,123)"); err != nil {
					t.Fatal(err)
				}
			}
			maintainAutomationCards(context.Background())
			wantCalls := int32(1)
			wantState := "closed"
			if mode == "unknown" {
				wantState = "unknown"
			}
			if mode == "busy" {
				wantCalls, wantState = 0, "queued"
			}
			var state string
			if err := db.DB.QueryRow("SELECT retire_state FROM automation_card_lifecycle WHERE card_id=123").Scan(&state); err != nil {
				t.Fatal(err)
			}
			if a.deleted.Load() != wantCalls || state != wantState {
				t.Fatal("unexpected retirement result", mode, a.deleted.Load(), state)
			}
			dueAgain()
			maintainAutomationCards(context.Background())
			if a.deleted.Load() != wantCalls {
				t.Fatal("retirement was repeated", mode, a.deleted.Load())
			}
			if mode == "success" && len(localCardIDs(readLocalSettings())) != 0 {
				t.Fatal("closed card remained selected")
			}
		})
	}
}

func TestProductRankingUsesRecentSuccessAndEligibility(t *testing.T) {
	newLocalFixture(t)
	now := time.Now().Unix()
	day := time.Unix(now, 0).UTC().Add(7 * time.Hour).Format("2006-01-02")
	if _, err := db.DB.Exec("INSERT INTO automation_product_daily(day,product_code,bin,successes,declines,attempts,updated_at) VALUES(?,?,?,?,?,?,?),(?,?,?,?,?,?,?)", day, "GOOD", "111111", 8, 2, 10, now, day, "WEAK", "222222", 2, 8, 10, now); err != nil {
		t.Fatal(err)
	}
	fee, min, max, recharge := 1.0, 20.0, 1000.0, 0.0
	restricted := []string{}
	blocked := []string{"OPENAI"}
	disabled := false
	products := []cardplatform.AutomationProduct{
		{Code: "WEAK", Bin: "222222", OpenFee: &fee, RechargeFee: &recharge, Min: &min, Max: &max, Restricted: &restricted},
		{Code: "GOOD", Bin: "111111", OpenFee: &fee, RechargeFee: &recharge, Min: &min, Max: &max, Restricted: &restricted},
		{Code: "OFF", Enabled: &disabled, OpenFee: &fee, RechargeFee: &recharge, Min: &min, Max: &max, Restricted: &restricted},
		{Code: "BLOCKED", OpenFee: &fee, RechargeFee: &recharge, Min: &min, Max: &max, Restricted: &blocked},
	}
	ranked, err := rankedAutomationProducts(products, 2100, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranked) != 2 || ranked[0].Product.Code != "GOOD" || ranked[1].Product.Code != "WEAK" {
		t.Fatal("wrong product ranking", ranked)
	}
	selected, mode, err := selectAutomationProduct(products, 2100, now)
	if err != nil || selected.Code != "GOOD" || mode != "best" {
		t.Fatal("highest-success product was not selected", selected.Code, mode, err)
	}
}

func TestAutomationCardTypeClassification(t *testing.T) {
	for input, want := range map[string]string{
		"one": "channel1", "ch1": "channel1", "four": "starlink", "starlink": "starlink", "星链卡": "starlink",
	} {
		if got := automationCardType(input); got != want {
			t.Fatal("wrong card type", input, got, want)
		}
	}
}

func TestProductRankingPrefersProvenWinsOverUntestedTie(t *testing.T) {
	newLocalFixture(t)
	now := time.Now().Unix()
	day := time.Unix(now, 0).UTC().Add(7 * time.Hour).Format("2006-01-02")
	if _, err := db.DB.Exec("INSERT INTO automation_product_daily(day,product_code,bin,successes,declines,attempts,updated_at) VALUES(?,?,?,?,?,?,?)", day, "PROVEN", "111111", 1, 0, 2, now); err != nil {
		t.Fatal(err)
	}
	fee, min, max, recharge := 1.0, 20.0, 1000.0, 0.0
	restricted := []string{}
	products := []cardplatform.AutomationProduct{
		{Code: "UNTESTED", Bin: "222222", OpenFee: &fee, RechargeFee: &recharge, Min: &min, Max: &max, Restricted: &restricted},
		{Code: "PROVEN", Bin: "111111", OpenFee: &fee, RechargeFee: &recharge, Min: &min, Max: &max, Restricted: &restricted},
	}
	selected, mode, err := selectAutomationProduct(products, 2100, now)
	if err != nil || selected.Code != "PROVEN" || mode != "best" {
		t.Fatal("untested product outranked proven wins", selected.Code, mode, err)
	}
}

func TestProductRankingSharesHistoryAcrossSameCardHead(t *testing.T) {
	newLocalFixture(t)
	now := time.Now().Unix()
	day := time.Unix(now, 0).UTC().Add(7 * time.Hour).Format("2006-01-02")
	if _, err := db.DB.Exec("INSERT INTO automation_product_daily(day,product_code,bin,successes,declines,attempts,updated_at) VALUES(?,?,?,?,?,?,?),(?,?,?,?,?,?,?)", day, "OLD_CODE", "111111", 8, 2, 10, now, day, "WEAK", "222222", 2, 8, 10, now); err != nil {
		t.Fatal(err)
	}
	fee, min, max, recharge := 1.0, 20.0, 1000.0, 0.0
	restricted := []string{}
	products := []cardplatform.AutomationProduct{
		{Code: "WEAK", Bin: "222222", OpenFee: &fee, RechargeFee: &recharge, Min: &min, Max: &max, Restricted: &restricted},
		{Code: "NEW_CODE", Bin: "111111", OpenFee: &fee, RechargeFee: &recharge, Min: &min, Max: &max, Restricted: &restricted},
	}
	ranked, err := rankedAutomationProducts(products, 2100, now)
	if err != nil || len(ranked) != 2 || ranked[0].Product.Code != "NEW_CODE" || ranked[0].Success != 8 {
		t.Fatal("same card head did not retain its success history", ranked, err)
	}
}

func TestProductRankingSeparatesSameBINAcrossCardTypes(t *testing.T) {
	newLocalFixture(t)
	now := time.Now().Unix()
	day := time.Unix(now, 0).UTC().Add(7 * time.Hour).Format("2006-01-02")
	if _, err := db.DB.Exec(`INSERT INTO automation_product_catalog(product_code,bin,card_type,first_seen,last_seen) VALUES
	 ('CHANNEL','555555','channel1',1,1),('STAR','555555','starlink',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO automation_product_daily(day,product_code,bin,successes,declines,attempts,updated_at) VALUES
	 (?,?,?,?,?,?,?),(?,?,?,?,?,?,?)`, day, "CHANNEL", "555555", 8, 2, 10, now, day, "STAR", "555555", 1, 9, 10, now); err != nil {
		t.Fatal(err)
	}
	fee, min, max, recharge := 1.0, 20.0, 1000.0, 0.0
	restricted := []string{}
	products := []cardplatform.AutomationProduct{
		{Code: "STAR", Bin: "555555", Issuer: "four", OpenFee: &fee, RechargeFee: &recharge, Min: &min, Max: &max, Restricted: &restricted},
		{Code: "CHANNEL", Bin: "555555", Issuer: "one", OpenFee: &fee, RechargeFee: &recharge, Min: &min, Max: &max, Restricted: &restricted},
	}
	ranked, err := rankedAutomationProducts(products, 2100, now)
	if err != nil || len(ranked) != 2 || ranked[0].Product.Code != "CHANNEL" || ranked[0].Success != 8 || ranked[1].Success != 1 {
		t.Fatal("same BIN from two card types was mixed", ranked, err)
	}
}

func TestProductSelectionExploresNewHeadsEveryFifthOpen(t *testing.T) {
	newLocalFixture(t)
	now := time.Now().Unix()
	day := time.Unix(now, 0).UTC().Add(7 * time.Hour).Format("2006-01-02")
	if _, err := db.DB.Exec("INSERT INTO automation_product_daily(day,product_code,bin,successes,declines,attempts,updated_at) VALUES(?,?,?,?,?,?,?)", day, "PROVEN", "111111", 8, 2, 10, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO automation_product_catalog(product_code,bin,card_type,first_seen,last_seen) VALUES
	 ('PROVEN','111111','channel1',1,1),('NEW_A','222222','channel1',1,1),('NEW_B','333333','starlink',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO automation_money(id,action,amount_minor,reserved_minor,state,created_at) VALUES
	 ('open-1','open',1,1,'verified',1),('open-2','open',1,1,'verified',2),
	 ('open-3','open',1,1,'verified',3),('open-4','open',1,1,'verified',4)`); err != nil {
		t.Fatal(err)
	}
	fee, min, max, recharge := 1.0, 20.0, 1000.0, 0.0
	restricted := []string{}
	products := []cardplatform.AutomationProduct{
		{Code: "PROVEN", Bin: "111111", Issuer: "one", OpenFee: &fee, RechargeFee: &recharge, Min: &min, Max: &max, Restricted: &restricted},
		{Code: "NEW_A", Bin: "222222", Issuer: "one", OpenFee: &fee, RechargeFee: &recharge, Min: &min, Max: &max, Restricted: &restricted},
		{Code: "NEW_B", Bin: "333333", Issuer: "four", OpenFee: &fee, RechargeFee: &recharge, Min: &min, Max: &max, Restricted: &restricted},
	}
	selected, mode, err := selectAutomationProduct(products, 2100, now)
	if err != nil || selected.Code != "NEW_B" || mode != "explore" {
		t.Fatal("fifth opening did not explore the less-sampled card type", selected.Code, mode, err)
	}
	if _, err = db.DB.Exec("INSERT INTO automation_product_selections(operation_id,product_code,bin,selection_mode,selected_at) VALUES('open-5','NEW_B','333333','explore',5)"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.DB.Exec(`INSERT INTO automation_money(id,action,amount_minor,reserved_minor,state,created_at) VALUES
	 ('open-5','open',1,1,'verified',5),('open-6','open',1,1,'verified',6),
	 ('open-7','open',1,1,'verified',7),('open-8','open',1,1,'verified',8),
	 ('open-9','open',1,1,'verified',9)`); err != nil {
		t.Fatal(err)
	}
	selected, mode, err = selectAutomationProduct(products, 2100, now)
	if err != nil || selected.Code != "NEW_A" || mode != "explore" {
		t.Fatal("exploration repeated one new head before trying another", selected.Code, mode, err)
	}
}
