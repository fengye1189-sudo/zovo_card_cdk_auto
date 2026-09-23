package cardplatform

import (
 "context"
 "encoding/json"
 "fmt"
 "math/big"
 "net/http"
 "regexp"
 "strconv"
 "strings"
)

var financeDecimalPattern=regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]{1,2})?$`)
func financeDecimal(s string)bool{return len(s)<=40&&financeDecimalPattern.MatchString(s)}

// Keep decimal amounts exact. Missing or sub-cent values are not silently rounded.
func FinanceMinor(raw json.RawMessage) (*int64,error) {
 s:=strings.Trim(string(raw),"\""); if s==""||s=="null" {return nil,nil}
 if !financeDecimal(s){return nil,fmt.Errorf("invalid amount")}
 r,ok:=new(big.Rat).SetString(s);if !ok{return nil,fmt.Errorf("invalid amount")}
 r.Mul(r,big.NewRat(100,1));if !r.IsInt()||!r.Num().IsInt64(){return nil,fmt.Errorf("non-cent amount")}
 n:=r.Num().Int64();if n < -100000000000 || n>100000000000{return nil,fmt.Errorf("amount out of range")};return &n,nil
}
type FinanceEntry struct {
 ID int64 `json:"id"`; CardID int64 `json:"card_id"`; RefID int64 `json:"ref_id"`
 Kind string `json:"type"`; Wallet string `json:"wallet"`; At string `json:"created_at"`
 Last4 string `json:"last4"`; Status string `json:"status"`; Direction string `json:"direction"`
 Amount *int64 `json:"amount_minor"`; Fee *int64 `json:"fee_minor"`; Before *int64 `json:"before_minor"`; After *int64 `json:"after_minor"`
 AmountRaw string `json:"amount_decimal"`; FeeRaw string `json:"fee_decimal"`; BeforeRaw string `json:"before_decimal"`; AfterRaw string `json:"after_decimal"`
}
type FinanceTransaction struct {
 ID string `json:"auth_id"`; Status string `json:"status"`; Kind string `json:"type"`
 At string `json:"auth_time"`; Merchant string `json:"merchant_name"`
 Authorized *json.Number `json:"auth_amount"`; AuthCurrency string `json:"auth_currency"`
 Settled *json.Number `json:"settle_amount"`; SettleCurrency string `json:"settle_currency"`
}
func (c *Client) FinanceTransactions(ctx context.Context,card int64,page int)([]FinanceTransaction,error){
 if card<=0||page<1||page>10000{return nil,fmt.Errorf("invalid transaction page")}
 raw,e:=c.doOpenAPI(ctx,http.MethodGet,fmt.Sprintf("/cards/%d/transactions?page=%d&page_size=50&sync=0",card,page),nil,"");if e!=nil{return nil,e}
 var list []FinanceTransaction;if json.Unmarshal(raw,&list)!=nil||list==nil||len(list)>50{return nil,fmt.Errorf("invalid transactions")}
 for i:=range list{r:=&list[i];if r.ID==""||len(r.ID)>150||r.Kind==""{return nil,fmt.Errorf("missing transaction identity")}
  for _,n:=range []*json.Number{r.Authorized,r.Settled}{if n==nil{continue};if !financeDecimal(n.String()){return nil,fmt.Errorf("invalid transaction amount")}}
  r.Merchant=adminText(r.Merchant);r.Status=adminText(r.Status);r.Kind=adminText(r.Kind);r.At=adminText(r.At);r.AuthCurrency=adminText(r.AuthCurrency);r.SettleCurrency=adminText(r.SettleCurrency)
 }
 return list,nil
}
func (c *Client) FinancePage(ctx context.Context,source string,card int64,page int)([]FinanceEntry,int,error){
 path:="";switch source{case "wallet":if page<1||page>10000{return nil,0,fmt.Errorf("invalid page")};path=fmt.Sprintf("/balance-logs?page=%d&page_size=20",page)
 case "recharge","flow":if card<=0{return nil,0,fmt.Errorf("invalid card")};suffix:="recharges";if source=="flow"{suffix="fund-flows"};path=fmt.Sprintf("/cards/%d/%s",card,suffix)
 default:return nil,0,fmt.Errorf("invalid source")}
 raw,e:=c.doOpenAPI(ctx,http.MethodGet,path,nil,"");if e!=nil{return nil,0,e}
 var list []json.RawMessage;total:=0
 if source=="wallet"{var v struct{List []json.RawMessage `json:"list"`;Total *int `json:"total"`};if json.Unmarshal(raw,&v)!=nil||v.List==nil||v.Total==nil||*v.Total<0{return nil,0,fmt.Errorf("invalid ledger")};list=v.List;total=*v.Total
 }else{if json.Unmarshal(raw,&list)!=nil||list==nil{return nil,0,fmt.Errorf("invalid ledger")};total=len(list)}
 if len(list)>5000{return nil,0,fmt.Errorf("ledger exceeds scan limit")}
 out:=[]FinanceEntry{};seen:=map[int64]bool{}
 for _,item:=range list{
  var v struct{ID int64 `json:"id"`;Card int64 `json:"card_id"`;Ref int64 `json:"ref_id"`;Kind string `json:"type"`;Wallet string `json:"wallet"`;At string `json:"created_at"`;Time string `json:"time"`;PAN string `json:"card_number"`;Status string `json:"status"`;Direction string `json:"direction"`;Amount json.RawMessage `json:"amount"`;Fee json.RawMessage `json:"fee"`;Before json.RawMessage `json:"before"`;After json.RawMessage `json:"after"`}
  if json.Unmarshal(item,&v)!=nil||v.ID<=0||seen[v.ID]{return nil,0,fmt.Errorf("invalid ledger identity")};seen[v.ID]=true
  if source!="wallet" {if v.Card!=0&&v.Card!=card{return nil,0,fmt.Errorf("card mismatch")};v.Card=card}
  x:=FinanceEntry{ID:v.ID,CardID:v.Card,RefID:v.Ref,Kind:adminText(v.Kind),Wallet:adminText(v.Wallet),At:adminText(v.At),Status:adminText(v.Status),Direction:adminText(v.Direction)}
  if x.At==""{x.At=adminText(v.Time)}
  if len(v.PAN)>=4{tail:=v.PAN[len(v.PAN)-4:];if _,e:=strconv.Atoi(tail);e==nil{x.Last4=tail}}
  targets:=[]**int64{&x.Amount,&x.Fee,&x.Before,&x.After};originals:=[]*string{&x.AmountRaw,&x.FeeRaw,&x.BeforeRaw,&x.AfterRaw}
  for i,b:=range []json.RawMessage{v.Amount,v.Fee,v.Before,v.After}{s:=strings.Trim(string(b),"\"");if s==""||s=="null"{continue};if !financeDecimal(s){return nil,0,fmt.Errorf("invalid decimal")};*originals[i]=s;n,e:=FinanceMinor(b);if e==nil{*targets[i]=n}}
  out=append(out,x)
 }
 return out,total,nil
}
