package subscriptionautomation

import "strings"

type TransitionInput struct {
	CurrentProvider string
	RawStatus string
	PrimaryConfirmedFailures int
	JZUsed bool
}

type TransitionDecision struct {
	Action string
	NextProvider string
	PrimaryConfirmedFailures int
	JZUsed bool
	OrderStatus string
}

// DecideTransition is pure and has no network/database side effects. It is the
// single source of truth for provider switching.
func DecideTransition(in TransitionInput) TransitionDecision {
	providerName := strings.ToLower(strings.TrimSpace(in.CurrentProvider))
	status := strings.ToLower(strings.TrimSpace(in.RawStatus))
	out := TransitionDecision{Action: "WAIT", PrimaryConfirmedFailures: in.PrimaryConfirmedFailures, JZUsed: in.JZUsed, OrderStatus: "PROCESSING"}
	if status == "succeeded" { out.Action = "COMPLETE"; out.OrderStatus = "SUCCEEDED"; return out }
	if !isConfirmedTerminalFailure(status) { return out }
	if providerName == "jz" || providerName == "jzactivation" {
		out.Action = "MANUAL_REVIEW"; out.OrderStatus = "MANUAL_REVIEW"; out.JZUsed = true; return out
	}
	out.PrimaryConfirmedFailures++
	if out.PrimaryConfirmedFailures < 2 {
		out.Action = "TRY_OTHER_PRIMARY"
		if providerName == "zovo" { out.NextProvider = "orbitcard" } else { out.NextProvider = "zovo" }
		out.OrderStatus = "PENDING"
		return out
	}
	if !out.JZUsed {
		out.Action = "TRY_RESCUE"; out.NextProvider = "jz"; out.JZUsed = true; out.OrderStatus = "RESCUE_PENDING"; return out
	}
	out.Action = "MANUAL_REVIEW"; out.OrderStatus = "MANUAL_REVIEW"
	return out
}

func isConfirmedTerminalFailure(status string) bool {
	switch status { case "failed", "declined", "rejected", "unsupported": return true; default: return false }
}
