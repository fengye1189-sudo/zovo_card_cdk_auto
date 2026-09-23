package handler

import (
 "testing"
 "github.com/tuzi/cdk-recharge-system/internal/cardplatform"
)

func TestExistingCardPaymentExceptionIsNarrow(t *testing.T) {
 for _, mode := range []string{"plus","go","pro_20x","other_card","other_reason","frozen","missing","balance","exhausted","identity"} {
  t.Run(mode,func(t *testing.T){
   skip,remain,balance:=true,int64(5),28.0
   candidate:=cardplatform.DirectCandidate{CardID:290694,Skip:&skip,SkipReason:"渠道 星链卡 不可用于自动开卡",AvailableUSD:&balance,LightRemain:&remain}
   card:=cardplatform.CardChoice{ID:290694,Product:"HLXOG406P",Last4:"7128",Status:"ACTIVE",Balance:&balance}
   plan:="plus"
   switch mode {
   case "go","pro_20x": plan=mode
   case "other_card": candidate.CardID=123
   case "other_reason": candidate.SkipReason="卡片已冻结"
   case "frozen": card.Status="FROZEN"
   case "missing": card.ID=123
   case "balance": balance=1
   case "exhausted": remain=0
   case "identity": card.Product="OTHER"
   }
   result:=existingPaymentCandidate(candidate,[]cardplatform.CardChoice{card},plan)
   want:=mode=="plus"||mode=="go"
   if result.Usable(1600)!=want {t.Fatal("incorrect authorization",mode)}
   if !*candidate.Skip || candidate.SkipReason=="" {t.Fatal("mutated upstream funding eligibility")}
  })
 }
}
