package handler

import "github.com/tuzi/cdk-recharge-system/internal/cardplatform"

// Owner-authorized compatibility for existing card 7128 only. The schedule
// incorrectly applies channel opening eligibility to this existing card.
// Never use this exception to authorize opening, topping up, or Pro upgrades.
func existingPaymentCandidate(candidate cardplatform.DirectCandidate, inventory []cardplatform.CardChoice, plan string) cardplatform.DirectCandidate {
	if (plan != "plus" && plan != "go") || candidate.CardID != 290694 || candidate.Skip == nil || !*candidate.Skip || candidate.SkipReason != "渠道 星链卡 不可用于自动开卡" {
		return candidate
	}
	for _, card := range inventory {
		if card.ID != candidate.CardID || card.Product != "HLXOG406P" || card.Last4 != "7128" || card.Status != "ACTIVE" { continue }
		if _, ok := usdMinor(card.Balance); !ok { return candidate }
		allowed := false
		candidate.Skip, candidate.SkipReason, candidate.AvailableUSD = &allowed, "", card.Balance
		return candidate
	}
	return candidate
}
