package subscriptionautomation

import "fmt"

type SimulationStep struct {
	Provider     string `json:"provider"`
	InputStatus  string `json:"input_status"`
	Action       string `json:"action"`
	NextProvider string `json:"next_provider,omitempty"`
	OrderStatus  string `json:"order_status"`
}

type SimulationResult struct {
	Scenario    string           `json:"scenario"`
	Passed      bool             `json:"passed"`
	FinalStatus string           `json:"final_status"`
	Steps       []SimulationStep `json:"steps"`
}

// Simulate runs only the pure transition state machine. It never creates an
// order, calls a provider, touches the database, or consumes credentials.
func Simulate(scenario string) (SimulationResult, error) {
	sequence, ok := map[string][]struct{ provider, status string }{
		"zovo_success":         {{"zovo", "succeeded"}},
		"primary_failover":     {{"zovo", "failed"}, {"orbitcard", "succeeded"}},
		"rescue_success":       {{"orbitcard", "failed"}, {"zovo", "rejected"}, {"jz", "succeeded"}},
		"unknown_wait":         {{"orbitcard", "timeout"}, {"orbitcard", "processing"}, {"orbitcard", "succeeded"}},
		"rescue_manual_review": {{"zovo", "failed"}, {"orbitcard", "failed"}, {"jz", "failed"}},
	}[scenario]
	if !ok {
		return SimulationResult{}, fmt.Errorf("unknown simulation scenario")
	}
	result := SimulationResult{Scenario: scenario, Passed: true}
	failures, jzUsed := 0, false
	for _, item := range sequence {
		decision := DecideTransition(TransitionInput{CurrentProvider: item.provider, RawStatus: item.status, PrimaryConfirmedFailures: failures, JZUsed: jzUsed})
		result.Steps = append(result.Steps, SimulationStep{Provider: item.provider, InputStatus: item.status, Action: decision.Action, NextProvider: decision.NextProvider, OrderStatus: decision.OrderStatus})
		failures, jzUsed = decision.PrimaryConfirmedFailures, decision.JZUsed
		if decision.Action == "MANUAL_REVIEW" {
			result.FinalStatus = "MANUAL_REVIEW"
		}
		if decision.Action == "COMPLETE" {
			result.FinalStatus = "SUCCEEDED"
		}
	}
	if result.FinalStatus == "" {
		result.FinalStatus = "PROCESSING"
	}
	return result, nil
}
