package handler
import (
 "context"
 "errors"
 "strings"
 "testing"
 "github.com/tuzi/cdk-recharge-system/internal/cardplatform"
 "github.com/tuzi/cdk-recharge-system/internal/db"
)
func TestReadRetryBounded(t *testing.T){
 for _,tc:=range []struct{name string;err error;want int}{
  {"timeout",context.DeadlineExceeded,2},{"invalid",errors.New("secret response"),1},{"unavailable",cardplatform.ErrDirectPlanUnavailable,1},
 }{t.Run(tc.name,func(t *testing.T){n:=0;retryAutomationRead(context.Background(),func(context.Context)error{n++;return tc.err});if n!=tc.want{t.Fatal(n)};if strings.Contains(automationReadReason(tc.err),"secret"){t.Fatal("raw error leaked")}})}
 n:=0;err:=retryAutomationRead(context.Background(),func(context.Context)error{n++;if n==1{return context.DeadlineExceeded};return nil});if err!=nil||n!=2{t.Fatal("transient recovery")}
 ctx,cancel:=context.WithCancel(context.Background());cancel();n=0;retryAutomationRead(ctx,func(context.Context)error{n++;return nil});if n!=0{t.Fatal("cancelled request issued")}
}
func TestReadRecoveryClearsOnlyMatchingAlertsWithoutFunding(t *testing.T){
 a:=newAutoFixture(t);a.balance=25;putAutoPolicy(t,moneyPolicy())
 autoAlert("money",0,"Plus 通道或服务费不符合本站充值规则，未执行资金操作。")
 for _,key:=range []string{"money_pricing","money_fee","money_candidates"}{autoAlert(key,0,"old")}
 maintainAutomationCards(context.Background())
 var n int;db.DB.QueryRow("SELECT COUNT(*) FROM automation_alerts WHERE resolved=0").Scan(&n)
 if n!=0||a.money.Load()!=0{t.Fatal("healthy reads must clear without funding",n)}
 autoAlert("money",0,"资金结果不明，请核对")
 dueAgain();maintainAutomationCards(context.Background())
 db.DB.QueryRow("SELECT COUNT(*) FROM automation_alerts WHERE alert_key='money' AND resolved=0").Scan(&n);if n!=1{t.Fatal("unrelated money warning cleared")}
}
