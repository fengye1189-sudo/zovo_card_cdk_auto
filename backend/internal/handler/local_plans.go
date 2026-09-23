package handler

func supportedLocalPlan(plan string) bool {
	return plan == "plus" || plan == "go" || plan == "pro_5x" || plan == "pro_20x"
}

func localCodeLabel(plan string) string {
	switch plan {
	case "plus":
		return "PULS-"
	case "go":
		return "GO-"
	case "pro_5x":
		return "PRO5X-"
	case "pro_20x":
		return "PRO-"
	default:
		return ""
	}
}

func isProDedicatedPlan(plan string) bool { return plan == "pro_5x" || plan == "pro_20x" }

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
	case "pro_20x":
		s.Currency = "PHP"
		s.MaxAmountMinor = 950000
	default:
		s.Enabled = false
		s.MaxAmountMinor = 0
	}
	if s.MaxFeeMinor > 50 {
		s.MaxFeeMinor = 50
	}
	return s
}
