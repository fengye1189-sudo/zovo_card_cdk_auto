package cardplatform

import (
 "context"
 "encoding/json"
 "fmt"
 "net/http"
 "regexp"
 "strconv"
 "strings"
)

// Only public scalar fields are forwarded. Never forward raw order/event objects.
var adminTextSecrets = regexp.MustCompile(`(?i)(sk_[a-z0-9_-]+|eyJ[a-z0-9_.-]+|[0-9]{12,19}|(?:session|token|password|authorization|secret|cvv)["'\s:=]+[^\s,}]+)`)
func adminText(s string) string {
 s=adminTextSecrets.ReplaceAllString(s,"[已隐藏]")
 if len([]rune(s))>600 {s=string([]rune(s)[:600])+"…"};return s
}
func publicScalars(v map[string]any, keys string) map[string]any {
 out:=map[string]any{}
 for _,k:=range strings.Fields(keys) {switch x:=v[k].(type) {case string:out[k]=adminText(x);case float64,bool:out[k]=x}}
 return out
}
func PublicDirectOrder(v map[string]any) map[string]any {
 out:=publicScalars(v,"id client_request_id product plan status stage account_email email currency quoted_amount_minor final_amount_minor service_fee_minor service_fee_status funding_state funding_hold_status payment_status payment_fact renewal_status renewal_message will_renew subscription_will_renew message error_code created_at updated_at completed_at card_id")
 for _,k:=range []string{"card_last_four","card_number"} {if s,ok:=v[k].(string);ok && len(s)>=4 {tail:=s[len(s)-4:];if _,e:=strconv.Atoi(tail);e==nil {out["card_last_four"]=tail;break}}}
 return out
}
func (c *Client) DirectOrderList(ctx context.Context,page int) (map[string]any,error) {
 if page<1 || page>10000 {return nil,fmt.Errorf("invalid page")}
 raw,e:=c.doOpenAPI(ctx,http.MethodGet,fmt.Sprintf("/gpt-direct/orders?page=%d&page_size=20",page),nil,"");if e!=nil{return nil,e}
 var v struct {List []map[string]any `json:"list"`;Total *int64 `json:"total"`}
 if json.Unmarshal(raw,&v)!=nil || v.List==nil || v.Total==nil || *v.Total<0 || len(v.List)>20 {return nil,fmt.Errorf("invalid order list")}
 list:=[]map[string]any{};for _,o:=range v.List {list=append(list,PublicDirectOrder(o))}
 return map[string]any{"list":list,"total":*v.Total,"page":page,"page_size":20},nil
}
func (c *Client) DirectOrderDetail(ctx context.Context,id int64) (map[string]any,error) {
 return c.directOrderDetail(ctx,id,"")
}
func (c *Client) DirectBoundOrderDetail(ctx context.Context,id int64,request string) (map[string]any,error) {
 if request==""{return nil,fmt.Errorf("missing merchant request")};return c.directOrderDetail(ctx,id,request)
}
func (c *Client) directOrderDetail(ctx context.Context,id int64,request string) (map[string]any,error) {
 if id<=0{return nil,fmt.Errorf("invalid order id")}
 raw,e:=c.DirectOrderStatus(ctx,id);if e!=nil{return nil,e}
 var v struct {Order map[string]any `json:"order"`;Events []map[string]any `json:"events"`}
 if json.Unmarshal(raw,&v)!=nil || v.Order["id"]!=float64(id) || (request!="" && v.Order["client_request_id"]!=request) {return nil,fmt.Errorf("order mismatch")}
 events:=[]map[string]any{};for i,event:=range v.Events {if i>=200{break};events=append(events,publicScalars(event,"id created_at occurred_at category step public_code public_message status to_status payment_fact"))}
 return map[string]any{"order":PublicDirectOrder(v.Order),"events":events},nil
}
func (c *Client) DirectAdminAction(ctx context.Context,id int64,action string) (map[string]any,error) {
 if id<=0 || (action!="cancel" && action!="cancel-renewal") {return nil,fmt.Errorf("invalid action")}
 raw,e:=c.doOpenAPI(ctx,http.MethodPost,"/gpt-direct/orders/"+strconv.FormatInt(id,10)+"/"+action,map[string]any{},"");if e!=nil{return nil,e}
 var v map[string]any;if json.Unmarshal(raw,&v)!=nil {return nil,fmt.Errorf("invalid action result")}
 return publicScalars(v,"status renewal_status renewal_message will_renew active_until"),nil
}
