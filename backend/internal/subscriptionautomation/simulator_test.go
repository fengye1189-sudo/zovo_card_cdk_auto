package subscriptionautomation

import "testing"

func TestSimulationScenarios(t *testing.T) {
	cases := map[string]string{"zovo_success": "SUCCEEDED", "primary_failover": "SUCCEEDED", "rescue_success": "SUCCEEDED", "unknown_wait": "SUCCEEDED", "rescue_manual_review": "MANUAL_REVIEW"}
	for scenario, want := range cases {
		got, err := Simulate(scenario)
		if err != nil { t.Fatalf("%s: %v", scenario, err) }
		if got.FinalStatus != want { t.Fatalf("%s final=%s want=%s", scenario, got.FinalStatus, want) }
	}
}
