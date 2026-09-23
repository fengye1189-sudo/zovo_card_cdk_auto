package handler

import (
 "context"
 "errors"
 "strings"
 "testing"
 "github.com/tuzi/cdk-recharge-system/internal/cardplatform"
 "github.com/tuzi/cdk-recharge-system/internal/db"
)

func TestInventoryAlertReasonsAreSanitized(t *testing.T) {
 cases := []struct{ err error; want string }{
  {context.DeadlineExceeded,"超时"},
  {context.Canceled,"中断"},
  {&cardplatform.APIError{HTTPStatus:403,Code:401,Msg:"secret-test-token"},"HTTP 403"},
  {&inventoryScanError{"卡台返回 101 张卡，超过当前 100 张扫描上限"},"101"},
  {errors.New("secret-test-token"),"格式异常"},
 }
 for _, tc := range cases {
  msg:=inventoryAlertMessage(tc.err)
  if !strings.Contains(msg,tc.want) || strings.Contains(msg,"secret-test-token") { t.Fatal("unsafe or wrong explanation",msg) }
 }
}

func TestInventoryRecoveryPreservesOtherMoneyAlerts(t *testing.T) {
 teamFixture(t)
 if _,err:=db.DB.Exec("CREATE TABLE automation_alerts(alert_key TEXT PRIMARY KEY,local_id INTEGER,message TEXT,updated_at INTEGER,resolved INTEGER)"); err!=nil {t.Fatal(err)}
 autoAlert("money",0,legacyInventoryAlert)
 recordInventoryScan(context.DeadlineExceeded)
 var resolved int
 if err:=db.DB.QueryRow("SELECT resolved FROM automation_alerts WHERE alert_key='money'").Scan(&resolved); err!=nil || resolved!=1 {t.Fatal("legacy warning not migrated",err)}
 autoAlert("money",0,"存在结果未确认的资金操作")
 recordInventoryScan(nil)
 if err:=db.DB.QueryRow("SELECT resolved FROM automation_alerts WHERE alert_key='money_inventory'").Scan(&resolved); err!=nil || resolved!=1 {t.Fatal("recovered warning not resolved",err)}
 if err:=db.DB.QueryRow("SELECT resolved FROM automation_alerts WHERE alert_key='money'").Scan(&resolved); err!=nil || resolved!=0 {t.Fatal("unrelated money warning cleared",err)}
 recordInventoryScan(context.DeadlineExceeded)
 if err:=db.DB.QueryRow("SELECT resolved FROM automation_alerts WHERE alert_key='money_inventory'").Scan(&resolved); err!=nil || resolved!=0 {t.Fatal("recurring failure not reopened",err)}
}
