package handler

import (
 "encoding/json"
 "fmt"
 "testing"
 "github.com/gin-gonic/gin"
 "github.com/tuzi/cdk-recharge-system/internal/db"
)

func TestUpstreamOnlyAllowsMoreThanThreeButHonorsZero(t *testing.T){
 for _,exhausted:=range []bool{false,true}{t.Run(fmt.Sprint(exhausted),func(t *testing.T){
  f:=newLocalFixture(t);token,body:=f.ready(t)
  for i:=0;i<8;i++{if _,e:=db.DB.Exec("INSERT INTO local_cdks(code_hash,prefix,plan,status,expires_at,created_at,card_id) VALUES(?,?,'plus','consumed',0,0,123)",fmt.Sprint("old",i),"old");e!=nil{t.Fatal(e)}}
  cfg:=readLocalSettings();cfg.UseUpstreamCardLimit=true
  b,_:=json.Marshal(cfg);if e:=db.SetSetting("local_cdk_settings",string(b));e!=nil{t.Fatal(e)}
  if localPaymentLimit(cfg)!=0{t.Fatal("local limit still active")}
  if exhausted{f.poolMode="exhausted"}
  status,d:=f.call("/preflight",gin.H{"redemption_token":token,"credential":body["credential"]})
  if exhausted{if status!=409||f.calls.Load()!=0{t.Fatal("zero remaining accepted",status,d)};return}
  if status!=200{t.Fatal(status,d)}
  body["preflight_token"]=d["preflight_token"]
  status,d=f.call("/redeem",body);if status!=200&&status!=202{t.Fatal(status,d)}
  if f.calls.Load()!=1{t.Fatal("card over old cap blocked")}
 })}
}
