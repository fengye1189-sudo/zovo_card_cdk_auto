package handler

import (
 "context"
 "errors"
 "fmt"
 "net"
 "time"
 "github.com/tuzi/cdk-recharge-system/internal/cardplatform"
 "github.com/tuzi/cdk-recharge-system/internal/db"
)

// Only callers issuing read-only pricing/candidate GETs may use this helper.
// Never wrap funding, opening or payment submissions in it.
func retryAutomationRead(ctx context.Context,read func(context.Context)error)error{
 var err error
 for attempt:=0;attempt<2;attempt++{
  if ctx.Err()!=nil{return ctx.Err()}
  child,cancel:=context.WithTimeout(ctx,12*time.Second);err=read(child);cancel()
  if err==nil||!transientAutomationRead(err)||ctx.Err()!=nil{return err}
  if attempt==0{timer:=time.NewTimer(300*time.Millisecond);select{case <-ctx.Done():timer.Stop();return ctx.Err();case <-timer.C:}}
 }
 return err
}
func transientAutomationRead(err error)bool{
 var api *cardplatform.APIError;var network net.Error
 if errors.As(err,&api){return api.HTTPStatus>=500}
 return errors.Is(err,context.DeadlineExceeded)||errors.As(err,&network)
}
func automationReadReason(err error)string{
 var api *cardplatform.APIError;var network net.Error
 switch {
 case errors.Is(err,context.DeadlineExceeded):return "查询超时"
 case errors.Is(err,context.Canceled):return "本轮查询已中断"
 case errors.As(err,&api):return fmt.Sprintf("上游接口失败（HTTP %d，业务码 %d）",api.HTTPStatus,api.Code)
 case errors.As(err,&network):if network.Timeout(){return "查询超时"};return "连接上游失败"
 default:return "上游返回资料不完整或格式异常"
 }
}
func resolveLegacyMoneyAlert(message string){
 _,_=db.DB.Exec("UPDATE automation_alerts SET resolved=1 WHERE alert_key='money' AND message=?",message)
}
