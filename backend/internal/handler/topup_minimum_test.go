package handler

import (
 "context"
 "testing"
 "github.com/tuzi/cdk-recharge-system/internal/cardplatform"
 "github.com/tuzi/cdk-recharge-system/internal/db"
)

func TestAllRechargeMinimumsDoNotChangeOpening(t *testing.T) {
 min,max,fee,openFee:=20.0,50000.0,0.0,1.0
 restricted:=[]string{"GOOGLE"}
 p:=cardplatform.AutomationProduct{Code:"P5378OX",Min:&min,Max:&max,RechargeFee:&fee,OpenFee:&openFee,Restricted:&restricted}
 if cost,ok:=productCost(p,1375,false);!ok||cost!=1375{t.Fatalf("valid $13.75 recharge rejected: %d %v",cost,ok)}
 if _,ok:=productCost(p,999,false);ok{t.Fatal("below $10 recharge accepted")}
 if _,ok:=productCost(p,1375,true);ok{t.Fatal("opening minimum reduced")}
 p.Code="OTHER"
 if _,ok:=productCost(p,1375,false);!ok{t.Fatal("other product recharge should use $10 minimum")}
 for _,code:=range []string{"P5378OX","K2306VB","HLXOG406P","OTHER"}{
  p.Code=code
  if _,ok:=productCost(p,1000,false);!ok{t.Fatal(code,"$10 recharge rejected")}
  if _,ok:=productCost(p,999,false);ok{t.Fatal(code,"below minimum accepted")}
  if _,ok:=productCost(p,1374,true);ok{t.Fatal(code,"opening minimum reduced")}
  if _,ok:=productCost(p,5000001,false);ok{t.Fatal(code,"maximum bypassed")}
 }
 restricted=[]string{"GOOGLE CHATGPT"}
 if _,ok:=productCost(p,1374,false);ok{t.Fatal("merchant restriction bypassed")}
 restricted=[]string{}
 p.RechargeFee=nil
 if _,ok:=productCost(p,1374,false);ok{t.Fatal("missing fee accepted")}
}

func TestLowBalanceCardAutomaticallyTopsUpToSavedTarget(t *testing.T) {
 a:=newAutoFixture(t)
 a.balance=4.26
 a.product="K2306VB"
 a.minimum=20
 p:=moneyPolicy();p.Threshold=500;p.Target=1800;p.CardCeiling=2150;p.MaxOperation=2200
 putAutoPolicy(t,p)
 if err:=db.SetSetting("local_cdk_settings",`{"enabled":true,"card_ids":[123],"min_card_balance_minor":1800,"max_fee_minor":100}`);err!=nil{t.Fatal(err)}
 maintainAutomationCards(context.Background())
 if a.money.Load()!=1||a.balance!=18{t.Fatalf("topup missing: calls=%d balance=%v",a.money.Load(),a.balance)}
 var amount int64
 if err:=db.DB.QueryRow("SELECT amount_minor FROM automation_money WHERE action='topup'").Scan(&amount);err!=nil||amount!=1374{t.Fatalf("wrong amount %d %v",amount,err)}
 dueAgain();maintainAutomationCards(context.Background())
 if a.money.Load()!=1{t.Fatal("pending topup repeated")}
}
