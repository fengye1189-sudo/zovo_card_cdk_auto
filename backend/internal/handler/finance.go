package handler

import (
 "context"
 "encoding/json"
 "fmt"
 "strconv"
 "time"
 "strings"

 "github.com/gin-gonic/gin"
 "github.com/tuzi/cdk-recharge-system/internal/cardplatform"
 "github.com/tuzi/cdk-recharge-system/internal/db"
)

func financeScope() string {cfg:=cardplatform.LoadConfig();return localHash(cfg.SiteBase+"|"+cfg.APIKey)}
func saveFinanceTransactions(scope string,card int64,list []cardplatform.FinanceTransaction)error{
 tx,e:=db.DB.Begin();if e!=nil{return e};defer tx.Rollback();for _,r:=range list{raw,_:=json.Marshal(r);_,e=tx.Exec("INSERT INTO finance_transactions(scope,card_id,auth_id,kind,payload,imported_at) VALUES(?,?,?,?,?,?) ON CONFLICT(scope,card_id,auth_id,kind) DO UPDATE SET payload=excluded.payload,imported_at=excluded.imported_at",scope,card,r.ID,r.Kind,string(raw),time.Now().Unix());if e!=nil{return e}};return tx.Commit()
}
func saveFinance(scope,source string,card int64,entries []cardplatform.FinanceEntry) error {
 tx,e:=db.DB.Begin();if e!=nil{return e};defer tx.Rollback()
 for _,r:=range entries {
  state:="unlinked";if source=="wallet"&&r.Amount!=nil&&r.Before!=nil&&r.After!=nil{state="arithmetic_ok";if *r.Before+*r.Amount!=*r.After{state="mismatch"}}
  if source=="wallet"&&((r.Amount==nil&&r.AmountRaw!="")||(r.Before==nil&&r.BeforeRaw!="")||(r.After==nil&&r.AfterRaw!="")){state="precision_review"}
  raw,_:=json.Marshal(r)
  _,e=tx.Exec(`INSERT INTO finance_records(scope,source,card_key,upstream_id,card_id,ref_id,kind,payload,check_state,imported_at)
 VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(scope,source,card_key,upstream_id) DO UPDATE SET
 card_id=excluded.card_id,ref_id=excluded.ref_id,kind=excluded.kind,payload=excluded.payload,check_state=excluded.check_state,imported_at=excluded.imported_at`,scope,source,card,r.ID,r.CardID,r.RefID,r.Kind,string(raw),state,time.Now().Unix());if e!=nil{return e}
 };return tx.Commit()
}
// Read-only upstream calls. A wallet equation is not evidence tying a payment to
// one of our operations. Neither imports nor human notes unlock money or codes.
func syncFinance(ctx context.Context){
 p,_,e:=readAutomationPolicy();if e!=nil||!p.Sync{return};cfg:=cardplatform.LoadConfig();if cfg.APIKey==""{return}
 scope:=localHash(cfg.SiteBase+"|"+cfg.APIKey);now:=time.Now().Unix()
 _,e=db.DB.Exec("INSERT OR IGNORE INTO finance_sync(scope) VALUES(?)",scope);if e!=nil{return}
 claim,e:=db.DB.Exec("UPDATE finance_sync SET next_run=? WHERE scope=? AND next_run<=?",now+300,scope,now);if e!=nil{return};n,_:=claim.RowsAffected();if n!=1{return}
 fail:=func(){db.DB.Exec("UPDATE finance_sync SET last_error=? WHERE scope=?","部分流水尚未同步，后台将重试；不会执行资金操作。",scope);autoAlert("finance-sync",0,"资金流水同步未完整完成，请查看资金对账。")}
 cli:=cardplatform.New(cfg);var page int;var cursor int64
 if db.DB.QueryRow("SELECT next_page,card_cursor FROM finance_sync WHERE scope=?",scope).Scan(&page,&cursor)!=nil{return}
 latest,total,e:=cli.FinancePage(ctx,"wallet",0,1);if e!=nil||saveFinance(scope,"wallet",0,latest)!=nil{fail();return}
 if page<2{page=2};if (page-1)*20<total {rows,_,err:=cli.FinancePage(ctx,"wallet",0,page);if err!=nil||saveFinance(scope,"wallet",0,rows)!=nil{fail();return};page++}
 if (page-1)*20>=total||page>10000{page=2}
 db.DB.Exec("UPDATE finance_sync SET next_page=?,total=? WHERE scope=?",page,total,scope)
 // Rotate one card per cycle, including cards seen in historical wallet entries.
 ids:=localCardIDs(readLocalSettings());for _,id:=range ids{if id>0{db.DB.Exec("INSERT OR IGNORE INTO finance_cards(scope,card_id) VALUES(?,?)",scope,id)}}
 db.DB.Exec("INSERT OR IGNORE INTO finance_cards(scope,card_id) SELECT scope,card_id FROM finance_records WHERE scope=? AND card_id>0",scope)
 db.DB.Exec("INSERT OR IGNORE INTO finance_cards(scope,card_id) SELECT scope,CASE WHEN result_card_id>0 THEN result_card_id ELSE card_id END FROM automation_money WHERE scope=? AND (card_id>0 OR result_card_id>0)",scope)
 var card int64;_ = db.DB.QueryRow("SELECT card_id FROM finance_cards WHERE scope=? AND card_id>? ORDER BY card_id LIMIT 1",scope,cursor).Scan(&card)
 if card==0{_ = db.DB.QueryRow("SELECT card_id FROM finance_cards WHERE scope=? ORDER BY card_id LIMIT 1",scope).Scan(&card)}
 if card>0{
  cardKey:=fmt.Sprintf("finance-card:%s:%d",scope,card)
  complete:=false
  defer func(){if complete{autoResolve(cardKey)}else{autoAlert(cardKey,0,fmt.Sprintf("卡片 ID %d 的资金或消费流水未完整同步，后台会轮流重查。",card))}}()
  // Advance the card cursor even if one historical/closed card is unavailable.
  db.DB.Exec("UPDATE finance_sync SET card_cursor=? WHERE scope=?",card,scope)
  for _,source:=range []string{"recharge","flow"}{rows,_,err:=cli.FinancePage(ctx,source,card,1);if err!=nil||saveFinance(scope,source,card,rows)!=nil{fail();return}}
  list,err:=cli.FinanceTransactions(ctx,card,1);if err!=nil||saveFinanceTransactions(scope,card,list)!=nil{fail();return}
  db.DB.Exec("INSERT OR IGNORE INTO finance_transaction_scan(scope,card_id) VALUES(?,?)",scope,card)
  var history int;_ = db.DB.QueryRow("SELECT next_page FROM finance_transaction_scan WHERE scope=? AND card_id=?",scope,card).Scan(&history);if history<2{history=2}
  if len(list)==50{older,err:=cli.FinanceTransactions(ctx,card,history);if err!=nil||saveFinanceTransactions(scope,card,older)!=nil{fail();return};history++;if len(older)<50||history>10000{history=2}}else{history=2}
  db.DB.Exec("UPDATE finance_transaction_scan SET next_page=? WHERE scope=? AND card_id=?",history,scope,card)
  complete=true
 }
 db.DB.Exec("UPDATE finance_sync SET next_page=?,card_cursor=?,last_ok=?,last_error='',total=? WHERE scope=?",page,card,now,total,scope);autoResolve("finance-sync")
 var mismatches int;_ = db.DB.QueryRow("SELECT COUNT(*) FROM finance_records WHERE scope=? AND check_state='mismatch'",scope).Scan(&mismatches)
 if mismatches>0{autoAlert("finance-mismatch",0,"平台钱包流水存在金额不平记录，请到资金对账核查。")}else{autoResolve("finance-mismatch")}
}
func AdminFinance(c *gin.Context){
 c.Header("Cache-Control","no-store");scope:=financeScope();page,_:=strconv.Atoi(c.DefaultQuery("page","1"));if page<1||page>10000{localError(c,400,"页码无效");return}
 source:=c.DefaultQuery("source","wallet");if source=="transaction"{adminFinanceTransactions(c,scope,page);return};if source!="wallet"&&source!="recharge"&&source!="flow"{localError(c,400,"流水类型无效");return}
 state:=c.Query("state");if state!=""&&state!="mismatch"&&state!="arithmetic_ok"&&state!="unlinked"&&state!="precision_review"{localError(c,400,"状态无效");return}
 var total int;if db.DB.QueryRow("SELECT COUNT(*) FROM finance_records WHERE scope=? AND source=? AND (?='' OR check_state=?)",scope,source,state,state).Scan(&total)!=nil{localError(c,503,"流水暂不可用");return}
 rows,e:=db.DB.Query("SELECT payload,check_state,imported_at FROM finance_records WHERE scope=? AND source=? AND (?='' OR check_state=?) ORDER BY upstream_id DESC,card_key LIMIT 20 OFFSET ?",scope,source,state,state,(page-1)*20);if e!=nil{localError(c,503,"流水暂不可用");return}
 list:=[]gin.H{};for rows.Next(){var raw,status string;var at int64;if rows.Scan(&raw,&status,&at)==nil{var entry cardplatform.FinanceEntry;if json.Unmarshal([]byte(raw),&entry)==nil{list=append(list,gin.H{"entry":entry,"check_state":status,"imported_at":at})}}};rows.Close()
 var last,totalUp,next int64;var message string;_ = db.DB.QueryRow("SELECT last_ok,total,next_page,last_error FROM finance_sync WHERE scope=?",scope).Scan(&last,&totalUp,&next,&message)
 var imported int;_ = db.DB.QueryRow("SELECT COUNT(*) FROM finance_records WHERE scope=? AND source='wallet'",scope).Scan(&imported)
 reviews:=[]gin.H{};rows,e=db.DB.Query("SELECT operation_id,wallet_id,note,actor,reviewed_at FROM finance_reviews WHERE scope=? ORDER BY reviewed_at DESC LIMIT 100",scope);if e==nil{for rows.Next(){var id,note,actor string;var wallet,at int64;if rows.Scan(&id,&wallet,&note,&actor,&at)==nil{reviews=append(reviews,gin.H{"operation_id":id,"wallet_id":wallet,"note":note,"actor":actor,"reviewed_at":at})}};rows.Close()}
 c.JSON(200,gin.H{"list":list,"total":total,"last_sync":last,"upstream_total":totalUp,"imported_wallet":imported,"next_history_page":next,"sync_error":message,"reviews":reviews})
}
func adminFinanceTransactions(c *gin.Context,scope string,page int){
 var total int;db.DB.QueryRow("SELECT COUNT(*) FROM finance_transactions WHERE scope=?",scope).Scan(&total)
 rows,e:=db.DB.Query("SELECT card_id,payload,imported_at FROM finance_transactions WHERE scope=? ORDER BY imported_at DESC,card_id,auth_id,kind LIMIT 20 OFFSET ?",scope,(page-1)*20);if e!=nil{localError(c,503,"消费流水暂不可用");return};defer rows.Close()
 list:=[]gin.H{};for rows.Next(){var card,at int64;var raw string;if rows.Scan(&card,&raw,&at)==nil{var r cardplatform.FinanceTransaction;if json.Unmarshal([]byte(raw),&r)==nil{list=append(list,gin.H{"card_id":card,"entry":r,"imported_at":at})}}}
 c.JSON(200,gin.H{"list":list,"total":total,"reviews":[]any{}})
}
func AdminFinanceReview(c *gin.Context){
 var req struct{Operation string `json:"operation_id"`;Wallet int64 `json:"wallet_id"`;Note string `json:"note"`;Confirmed bool `json:"confirmed"`};if !localBody(c,&req){return}
 req.Note=strings.TrimSpace(req.Note);if !req.Confirmed||req.Wallet<=0||len(req.Operation)>100||len([]rune(req.Note))<5||len([]rune(req.Note))>300{localError(c,400,"请明确确认并填写核对依据（5 至 300 字）");return}
 scope:=financeScope();var card,cost int64;var action string
 e:=db.DB.QueryRow("SELECT CASE WHEN result_card_id>0 THEN result_card_id ELSE card_id END,reserved_minor,action FROM automation_money WHERE id=? AND scope=?",req.Operation,scope).Scan(&card,&cost,&action);if e!=nil||card<=0{localError(c,409,"未找到当前通道对应的本站资金操作");return}
 var raw,state string;e=db.DB.QueryRow("SELECT payload,check_state FROM finance_records WHERE scope=? AND source='wallet' AND card_key=0 AND upstream_id=?",scope,req.Wallet).Scan(&raw,&state)
 var entry cardplatform.FinanceEntry;kind:="card_recharge";if action=="open"||action=="pro5x_reserve_open"{kind="open_card"}
 if action=="pro_open" {var fee int64;if err:=db.DB.QueryRow("SELECT api_fee_minor FROM pro_dedicated_orders WHERE money_id=?",req.Operation).Scan(&fee);err!=nil{localError(c,409,"专卡费用记录缺失，请核对");return};kind="open_card";cost-=fee}
 if e!=nil||json.Unmarshal([]byte(raw),&entry)!=nil||state!="arithmetic_ok"||entry.CardID!=card||entry.Kind!=kind||entry.Amount==nil||*entry.Amount!=-cost{localError(c,409,"卡片、流水类型或总金额不一致，不能关联；请到 Zovo 核查分拆扣费等情况");return}
 // Notes are public admin text, never a storage channel for credentials.
 safe:=cardplatform.PublicDirectOrder(map[string]any{"message":req.Note})["message"].(string)
 _,e=db.DB.Exec("INSERT INTO finance_reviews(operation_id,scope,wallet_id,note,actor,reviewed_at) VALUES(?,?,?,?,?,?)",req.Operation,scope,req.Wallet,safe,c.GetString("username"),time.Now().Unix());if e!=nil{localError(c,409,"该操作或流水已有关联记录，不能重复关联");return}
 db.WriteAudit(c.GetString("username"),"finance_review",fmt.Sprintf("operation=%s wallet=%d; no financial state changed",req.Operation,req.Wallet),c.ClientIP());c.JSON(200,gin.H{"message":"人工核对备注已保存；未解锁操作、退款或重试付款"})
}
