package subscriptionautomation

import "testing"

func TestSelectPrimaryBalancesWithoutJZ(t *testing.T) {
	decision, err := SelectPrimary(map[string]int{"zovo": 5, "orbitcard": 2, "jz": 0})
	if err != nil { t.Fatal(err) }
	if decision.Provider != "orbitcard" || decision.Role != "PRIMARY" { t.Fatalf("unexpected decision: %#v", decision) }
	if decision.Provider == "jz" { t.Fatal("rescue provider joined normal routing") }
}

func TestSelectPrimaryIsDeterministicOnTie(t *testing.T) {
	decision, err := SelectPrimary(map[string]int{"zovo": 4, "orbitcard": 4})
	if err != nil { t.Fatal(err) }
	if decision.Provider != "orbitcard" { t.Fatalf("tie decision=%q", decision.Provider) }
}
