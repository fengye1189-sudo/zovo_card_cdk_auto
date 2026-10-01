package handler

var localPlanKeys = []string{
	"plus", "go", "pro_5x", "pro_5x_cl", "pro_20x",
	"credit250", "credit500", "credit1000", "credit2500", "credit5000", "credit25000",
}

func supportedLocalPlan(plan string) bool {
	for _, key := range localPlanKeys {
		if plan == key {
			return true
		}
	}
	return false
}

func upstreamLocalPlan(plan string) string {
	if plan == "pro_5x_cl" {
		return "pro_5x"
	}
	return plan
}

func localPlanPaymentRegion(plan string) (string, string) {
	if plan == "pro_5x_cl" {
		return "CL", "CLP"
	}
	return "PH", "PHP"
}

func localCodeLabel(plan string) string {
	switch plan {
	case "plus":
		return "PULS-"
	case "go":
		return "GO-"
	case "pro_5x":
		return "PRO5X-"
	case "pro_5x_cl":
		return "PRO5CL-"
	case "pro_20x":
		return "PRO-"
	case "credit250":
		return "CODEX250-"
	case "credit500":
		return "CODEX500-"
	case "credit1000":
		return "CODEX1000-"
	case "credit2500":
		return "CODEX2500-"
	case "credit5000":
		return "CODEX5000-"
	case "credit25000":
		return "CODEX25000-"
	default:
		return ""
	}
}

func isPro5xDedicatedPlan(plan string) bool { return plan == "pro_5x" || plan == "pro_5x_cl" }

func isProDedicatedPlan(plan string) bool { return isPro5xDedicatedPlan(plan) || plan == "pro_20x" }

// Owner-authorized per-plan subscription caps, in PHP centavos. These do not
// increase card topups, opening amounts or daily automation budgets.
func settingsForLocalPlan(s localSettings, plan string) localSettings {
	switch plan {
	case "plus":
		return s
	case "go":
		s.Currency = "PHP"
		s.MaxAmountMinor = 35000
	case "pro_5x":
		s.Currency = "PHP"
		s.MaxAmountMinor = 650000
	case "pro_5x_cl":
		s.Currency = "CLP"
		s.MaxAmountMinor = 150000
	case "pro_20x":
		s.Currency = "PHP"
		s.MaxAmountMinor = 950000
	case "credit250":
		s.Currency = "PHP"
		s.MaxAmountMinor = 65000
	case "credit500":
		s.Currency = "PHP"
		s.MaxAmountMinor = 125000
	case "credit1000":
		s.Currency = "PHP"
		s.MaxAmountMinor = 250000
	case "credit2500":
		s.Currency = "PHP"
		s.MaxAmountMinor = 620000
	case "credit5000":
		s.Currency = "PHP"
		s.MaxAmountMinor = 1250000
	case "credit25000":
		s.Currency = "PHP"
		s.MaxAmountMinor = 6200000
	default:
		s.Enabled = false
		s.MaxAmountMinor = 0
	}
	if s.MaxFeeMinor > 50 {
		s.MaxFeeMinor = 50
	}
	return s
}
