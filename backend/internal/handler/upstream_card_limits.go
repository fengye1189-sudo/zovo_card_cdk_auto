package handler

import (
 "context"
 "github.com/tuzi/cdk-recharge-system/internal/cardplatform"
)

// A shared card is exhausted only when every supported plan explicitly reports
// zero. Missing data, -1 or any remaining capacity must not retire that card.
func upstreamExhaustedCards(ctx context.Context,cli *cardplatform.Client) (map[int64]bool,error) {
 counts:=map[int64]int{}
 for _,plan:=range []string{"plus","go","pro_20x"} {
  cards,e:=cli.DirectCandidatesForPlan(ctx,plan);if e!=nil{return nil,e}
  for _,card:=range cards {if card.LightRemain!=nil&&*card.LightRemain==0{counts[card.CardID]++}}
 }
 result:=map[int64]bool{}
 for id,n:=range counts{if n==3{result[id]=true}}
 return result,nil
}
