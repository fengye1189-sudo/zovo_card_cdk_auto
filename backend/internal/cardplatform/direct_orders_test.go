package cardplatform

import (
 "context"
 "encoding/json"
 "net/http"
 "net/http/httptest"
 "testing"
)

func TestDirectPricingFailsClosed(t *testing.T) {
 for _,mode:=range []string{"valid","registry_missing","purchasable_missing","purchasable_false","product_wrong","mapping_missing","enabled_missing","enabled_false","fee_missing","fee_negative","version_missing","duplicate"} {
  t.Run(mode,func(t *testing.T){
   p:=map[string]any{"enabled":true,"serviceFeeUsdMinor":15}
   item:=map[string]any{"key":"plus","product":"gpt","acc_plan_key":"plus","purchasable":true}
   v:=map[string]any{"version":214,"plans":map[string]any{"plus":p},"registry":[]any{item}}
   switch mode {
   case "registry_missing":delete(v,"registry")
   case "purchasable_missing":delete(item,"purchasable")
   case "purchasable_false":item["purchasable"]=false
   case "product_wrong":item["product"]="claude"
   case "mapping_missing":delete(item,"acc_plan_key")
   case "enabled_missing":delete(p,"enabled")
   case "enabled_false":p["enabled"]=false
   case "fee_missing":delete(p,"serviceFeeUsdMinor")
   case "fee_negative":p["serviceFeeUsdMinor"]=-1
   case "version_missing":delete(v,"version")
   case "duplicate":v["registry"]=[]any{item,item}
   }
   srv:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){json.NewEncoder(w).Encode(map[string]any{"code":0,"data":v})}));defer srv.Close()
   t.Setenv("CARD_API_BASE",srv.URL);t.Setenv("CARD_API_KEY","test-key")
   version,fee,err:=New(Config{SiteBase:srv.URL,APIKey:"test-key"}).DirectPricing(context.Background(),"plus")
   if mode=="valid" {if err!=nil || version!=214 || fee!=15 {t.Fatalf("valid pricing: %d %d %v",version,fee,err)}} else if err==nil {t.Fatal("incomplete eligibility accepted")}
  })
 }
}
