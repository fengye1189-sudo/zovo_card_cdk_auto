package handler

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
	"strings"
	"testing"
)

func TestLocalPlanFulfillmentAndCaps(t *testing.T) {
	for _, plan := range []string{"go", "pro_20x"} {
		for _, mode := range []string{"at-limit", "over-limit", "wrong-currency"} {
			t.Run(plan+"/"+mode, func(t *testing.T) {
				f := newLocalFixture(t)
				f.plan = plan
				f.quoteCurrency = "PHP"
				cfg := localSettings{Enabled: true, CardID: 123, MinCardBalanceMinor: 2500, Currency: "USD", MaxAmountMinor: 3000, MaxFeeMinor: 50}
				b, _ := json.Marshal(cfg)
				if e := db.SetSetting("local_cdk_settings", string(b)); e != nil {
					t.Fatal(e)
				}
				cap := settingsForLocalPlan(cfg, plan).MaxAmountMinor
				f.quoteAmount = cap
				if mode == "over-limit" {
					f.quoteAmount++
				}
				if mode == "wrong-currency" {
					f.quoteCurrency = "USD"
				}
				status, d := f.call("/issue", gin.H{"product_id": plan, "count": 1, "days": 30, "request_id": "batch-plan-0123456789012345"})
				if status != 200 {
					t.Fatal(status, d)
				}
				status, d = f.call("/preview", gin.H{"code": d["codes"].([]any)[0]})
				if status != 200 || d["plan"] != plan {
					t.Fatal(status, d)
				}
				token := d["redemption_token"]
				cred := gin.H{"mode": "session", "session": strings.Repeat("mock-only-", 8)}
				status, d = f.call("/preflight", gin.H{"redemption_token": token, "credential": cred})
				if mode != "at-limit" {
					if status != 409 || f.calls.Load() != 0 {
						t.Fatal("invalid quote accepted", status, d)
					}
					return
				}
				if status != 200 || d["plan"] != plan {
					t.Fatal(status, d)
				}
				body := gin.H{"redemption_token": token, "preflight_token": d["preflight_token"], "credential": cred, "confirmed": true}
				status, d = f.call("/redeem", body)
				if status != 202 && status != 200 {
					t.Fatal(status, d)
				}
				f.call("/redeem", body)
				if f.calls.Load() != 1 {
					t.Fatal("duplicate or missing payment", f.calls.Load())
				}
				if readLocalSettings().MaxAmountMinor != 3000 {
					t.Fatal("global limit mutated")
				}
			})
		}
	}
}

func TestMultiPlanMigrationPreservesProducts(t *testing.T) {
	newLocalFixture(t)
	if _, e := db.DB.Exec("UPDATE operations_products SET enabled=0,name='Original name' WHERE id='plus'"); e != nil {
		t.Fatal(e)
	}
	if e := InitOperationsProducts(); e != nil {
		t.Fatal(e)
	}
	var name string
	var enabled bool
	if e := db.DB.QueryRow("SELECT name,enabled FROM operations_products WHERE id='plus'").Scan(&name, &enabled); e != nil || name != "Original name" || enabled {
		t.Fatal("existing product changed", e)
	}
	if settingsForLocalPlan(localSettings{MaxAmountMinor: 100000}, "plus").MaxAmountMinor != 100000 {
		t.Fatal("Plus changed")
	}
	var plan string
	if e := db.DB.QueryRow("SELECT plan FROM operations_products WHERE id='pro_5x'").Scan(&plan); e != nil || plan != "pro_5x" {
		t.Fatal("Pro 5X product missing", e)
	}
	if got := settingsForLocalPlan(localSettings{Enabled: true, MaxAmountMinor: 100000, MaxFeeMinor: 50}, "pro_5x"); got.Currency != "PHP" || got.MaxAmountMinor != 650000 || got.MaxFeeMinor != 50 {
		t.Fatal("Pro 5X cap incorrect", got)
	}
	if localCodeLabel("pro_5x") != "PRO5X-" {
		t.Fatal("Pro 5X code prefix incorrect")
	}
}

func TestPro5xMigrationPreservesExistingCatalog(t *testing.T) {
	newLocalFixture(t)
	_, err := db.DB.Exec(`DROP TABLE operations_products;
	 CREATE TABLE operations_products (
	 id TEXT PRIMARY KEY,name TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',
	 plan TEXT NOT NULL CHECK(plan IN ('plus','go','pro_20x')),enabled INTEGER NOT NULL DEFAULT 1,
	 default_days INTEGER NOT NULL DEFAULT 90,reference_price_minor INTEGER NOT NULL DEFAULT 0,
	 currency TEXT NOT NULL DEFAULT 'USD',version INTEGER NOT NULL DEFAULT 1,updated_at INTEGER NOT NULL);
	 INSERT INTO operations_products(id,name,plan,enabled,default_days,reference_price_minor,currency,version,updated_at)
	 VALUES('plus','Owner Plus','plus',0,90,1234,'USD',7,10),
	 ('go','Owner Go','go',1,90,2345,'PHP',8,11),
	 ('pro_20x','Owner Pro','pro_20x',1,90,3456,'PHP',9,12);`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := migrateLocalPlanProducts(); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM operations_products").Scan(&count); err != nil || count != 4 {
		t.Fatal("migration changed catalog count", count, err)
	}
	var name, plan, currency string
	var enabled, days, price, version int
	if err := db.DB.QueryRow("SELECT name,plan,enabled,default_days,reference_price_minor,currency,version FROM operations_products WHERE id='plus'").Scan(&name, &plan, &enabled, &days, &price, &currency, &version); err != nil || name != "Owner Plus" || plan != "plus" || enabled != 0 || days != 90 || price != 1234 || currency != "USD" || version != 7 {
		t.Fatal("existing product changed", name, plan, enabled, days, price, currency, version, err)
	}
	if err := db.DB.QueryRow("SELECT name,plan,currency FROM operations_products WHERE id='pro_5x'").Scan(&name, &plan, &currency); err != nil || name != "GPTPRO5x卡冲升级" || plan != "pro_5x" || currency != "PHP" {
		t.Fatal("Pro 5X product not added", name, plan, currency, err)
	}
}
