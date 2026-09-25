package handler

import (
 "encoding/json"
 "net/http"
 "net/http/httptest"
 "strings"
 "sync"
 "sync/atomic"
 "testing"

 "github.com/gin-gonic/gin"
 "github.com/tuzi/cdk-recharge-system/internal/db"
)

func directFixture(t *testing.T,status,renewal string,fail bool) (*localFixture,*atomic.Int32) {
 f:=newLocalFixture(t);posts:=&atomic.Int32{}
 upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  if r.Header.Get("X-API-Key")!="test-key" {t.Error("missing key")}
  order:=gin.H{"id":77,"product":"gpt","status":status,"renewal_status":renewal,"card_number":"5378721234567128","session":"secret-session","cvv":"987","message":"token: hidden-secret","client_request_id":"merchant-77"}
  if r.Method=="POST" {posts.Add(1);if fail {w.WriteHeader(502);w.Write([]byte("secret raw upstream"));return};json.NewEncoder(w).Encode(gin.H{"code":0,"data":gin.H{"renewal_status":"pending","session":"secret-session"}});return}
  if r.URL.Path=="/openapi/v1/gpt-direct/orders" {json.NewEncoder(w).Encode(gin.H{"code":0,"data":gin.H{"list":[]any{order},"total":1}});return}
  json.NewEncoder(w).Encode(gin.H{"code":0,"data":gin.H{"order":order,"events":[]any{gin.H{"public_message":"token: hidden-event","credential":"secret-event","created_at":"2026-09-08T10:00:00Z"}}}})
 }));t.Cleanup(upstream.Close);t.Setenv("CARD_API_BASE",upstream.URL)
 f.router.GET("/orders",AdminDirectOrders);f.router.GET("/orders/:id",AdminDirectDetail);f.router.POST("/orders/:id/:action",AdminDirectAction)
 return f,posts
}
func TestDirectAdminReadMasking(t *testing.T){
 f,posts:=directFixture(t,"completed","warning",false)
 for _,path:=range []string{"/orders","/orders/77"} {
  w:=httptest.NewRecorder();f.router.ServeHTTP(w,httptest.NewRequest("GET",path,nil))
  if w.Code!=200 || w.Header().Get("Cache-Control")!="no-store" {t.Fatal(w.Code)}
  for _,secret:=range []string{"5378721234567128","secret-session","987","hidden-secret","hidden-event","secret-event"} {if strings.Contains(w.Body.String(),secret){t.Fatal("unmasked field",secret)}}
  if !strings.Contains(w.Body.String(),"7128"){t.Fatal("tail missing")}
 }
 w:=httptest.NewRecorder();f.router.ServeHTTP(w,httptest.NewRequest("GET","/orders/78",nil));if w.Code!=502{t.Fatal("mismatched order accepted")}
 if posts.Load()!=0{t.Fatal("read mutated upstream")}
}
func TestDirectAdminMergesSignedWebhookOrders(t *testing.T){
 f,_:=directFixture(t,"completed","success",false)
 if _,err:=db.DB.Exec(`CREATE TABLE IF NOT EXISTS webhook_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,event_type TEXT NOT NULL,idem_key TEXT NOT NULL UNIQUE,
  payload TEXT NOT NULL,processed INTEGER NOT NULL DEFAULT 0,created_at DATETIME DEFAULT CURRENT_TIMESTAMP
 )`);err!=nil{t.Fatal(err)}
 payload:=`{"event":"gpt_direct.completed","order_id":88,"status":"completed","account_email":"web@example.com","product":"gpt","plan":"plus","session":"must-not-leak","card_number":"5378721234569999"}`
 if err:=db.InsertWebhookEvent("gpt_direct.completed","gpt_direct.completed|order|88",payload);err!=nil{t.Fatal(err)}
 w:=httptest.NewRecorder();f.router.ServeHTTP(w,httptest.NewRequest("GET","/orders",nil))
 if w.Code!=200{t.Fatal(w.Code,w.Body.String())}
 body:=w.Body.String()
 if !strings.Contains(body,`"id":88`)||!strings.Contains(body,`"synced_only":true`)||!strings.Contains(body,"web@example.com"){t.Fatal("synced web order missing",body)}
 if strings.Contains(body,"must-not-leak")||strings.Contains(body,"5378721234569999"){t.Fatal("webhook secret leaked")}
}
func TestDirectAdminActionGuards(t *testing.T){
 for _,tc:=range []struct{status,renewal,action string;allowed bool}{
  {"queued","","cancel",true},{"awaiting_card","","cancel",true},{"funding_pending","","cancel",true},
  {"running","","cancel",false},{"pending","","cancel",false},{"completed","warning","cancel",false},
  {"completed","warning","cancel-renewal",true},{"completed","pending","cancel-renewal",true},
  {"completed","success","cancel-renewal",false},{"completed","not_requested","cancel-renewal",false},{"queued","warning","cancel-renewal",false},
 }{t.Run(tc.status+tc.renewal+tc.action,func(t *testing.T){
  f,posts:=directFixture(t,tc.status,tc.renewal,false)
  path:="/orders/77/"+tc.action
  s,_:=f.call(path,gin.H{"expected_status":tc.status});if s!=400||posts.Load()!=0{t.Fatal("missing confirmation allowed")}
  s,_=f.call(path,gin.H{"confirmed":true,"expected_status":"stale"});if s!=409||posts.Load()!=0{t.Fatal("stale status allowed")}
  s,_=f.call(path,gin.H{"confirmed":true,"expected_status":tc.status})
  if tc.allowed {if s!=200 || posts.Load()!=1{t.Fatalf("expected action: %d %d",s,posts.Load())};s,_=f.call(path,gin.H{"confirmed":true,"expected_status":tc.status});if s!=409||posts.Load()!=1{t.Fatal("repeat sent")}} else if s!=409 || posts.Load()!=0{t.Fatal("forbidden action sent")}
 })}
}
func TestDirectAdminUnknownNeverAutoRetries(t *testing.T){
 f,posts:=directFixture(t,"queued","",true)
 s,data:=f.call("/orders/77/cancel",gin.H{"confirmed":true,"expected_status":"queued"});if s!=502 || strings.Contains(data["error"].(string),"secret raw"){t.Fatal("bad error response")}
 // Even after a restart-equivalent read and cooldown, unknown outcome stays blocked.
 db.DB.Exec("UPDATE direct_admin_actions SET attempted_at=0")
 s,_=f.call("/orders/77/cancel",gin.H{"confirmed":true,"expected_status":"queued"});if s!=409||posts.Load()!=1{t.Fatal("unknown cancellation retried")}
}
func TestDirectAdminConcurrentAction(t *testing.T){
 f,posts:=directFixture(t,"completed","warning",false);var wg sync.WaitGroup
 for i:=0;i<5;i++ {wg.Add(1);go func(){defer wg.Done();f.call("/orders/77/cancel-renewal",gin.H{"confirmed":true,"expected_status":"completed"})}()};wg.Wait()
 if posts.Load()!=1{t.Fatal("concurrent duplicate",posts.Load())}
}
