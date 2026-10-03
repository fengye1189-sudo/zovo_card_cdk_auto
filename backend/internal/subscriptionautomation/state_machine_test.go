package subscriptionautomation

import "testing"

func TestUnknownNeverSwitchesProvider(t *testing.T) {
	for _, status := range []string{"pending", "processing", "running", "queued", "awaiting_captcha", "action_required", "timeout", "unknown"} {
		got := DecideTransition(TransitionInput{CurrentProvider: "zovo", RawStatus: status})
		if got.Action != "WAIT" || got.PrimaryConfirmedFailures != 0 {
			t.Fatalf("status %s switched provider: %#v", status, got)
		}
	}
}

func TestTwoPrimaryFailuresThenOneRescue(t *testing.T) {
	first := DecideTransition(TransitionInput{CurrentProvider: "zovo", RawStatus: "failed"})
	if first.Action != "TRY_OTHER_PRIMARY" || first.NextProvider != "orbitcard" || first.PrimaryConfirmedFailures != 1 {
		t.Fatalf("first=%#v", first)
	}
	second := DecideTransition(TransitionInput{CurrentProvider: "orbitcard", RawStatus: "rejected", PrimaryConfirmedFailures: first.PrimaryConfirmedFailures})
	if second.Action != "TRY_RESCUE" || second.NextProvider != "jz" || !second.JZUsed || second.PrimaryConfirmedFailures != 2 {
		t.Fatalf("second=%#v", second)
	}
	last := DecideTransition(TransitionInput{CurrentProvider: "jz", RawStatus: "failed", PrimaryConfirmedFailures: second.PrimaryConfirmedFailures, JZUsed: true})
	if last.Action != "MANUAL_REVIEW" {
		t.Fatalf("last=%#v", last)
	}
}

func TestUnavailablePrimaryIsExcluded(t *testing.T) {
	got, err := SelectPrimaryWithHealth(map[string]int{"zovo": 0, "orbitcard": 10}, map[string]HealthState{"zovo": HealthUnavailable, "orbitcard": HealthHealthy})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "orbitcard" {
		t.Fatalf("got=%#v", got)
	}
}
