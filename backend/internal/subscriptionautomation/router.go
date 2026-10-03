package subscriptionautomation

import (
	"fmt"
	"sort"
)

type RouteCandidate struct {
	Name        string `json:"name"`
	Role        string `json:"role"`
	Weight      int    `json:"weight"`
	RecentCount int    `json:"recent_count"`
}

type RouteDecision struct {
	Provider    string `json:"provider"`
	Role        string `json:"role"`
	Weight      int    `json:"weight"`
	RecentCount int    `json:"recent_count"`
	Window      int    `json:"window"`
}

type HealthState string

const (
	HealthHealthy     HealthState = "HEALTHY"
	HealthDegraded    HealthState = "DEGRADED"
	HealthUnavailable HealthState = "UNAVAILABLE"
)

// SelectPrimary uses a deterministic weighted least-used policy. JZ is not a
// candidate here; rescue selection is a separate state transition.
func SelectPrimary(counts map[string]int) (RouteDecision, error) {
	return SelectPrimaryWithHealth(counts, map[string]HealthState{"zovo": HealthHealthy, "orbitcard": HealthHealthy})
}

func SelectPrimaryWithHealth(counts map[string]int, health map[string]HealthState) (RouteDecision, error) {
	candidates := []RouteCandidate{
		{Name: "zovo", Role: "PRIMARY", Weight: 45, RecentCount: counts["zovo"]},
		{Name: "orbitcard", Role: "PRIMARY", Weight: 45, RecentCount: counts["orbitcard"]},
	}
	available := candidates[:0]
	for _, candidate := range candidates {
		state := health[candidate.Name]
		if state == "" {
			state = HealthDegraded
		}
		if state != HealthUnavailable {
			available = append(available, candidate)
		}
	}
	candidates = available
	sort.SliceStable(candidates, func(i, j int) bool {
		left := float64(candidates[i].RecentCount+1) / float64(candidates[i].Weight)
		right := float64(candidates[j].RecentCount+1) / float64(candidates[j].Weight)
		if left != right {
			return left < right
		}
		return candidates[i].Name < candidates[j].Name
	})
	if len(candidates) == 0 {
		return RouteDecision{}, fmt.Errorf("no primary provider configured")
	}
	selected := candidates[0]
	return RouteDecision{Provider: selected.Name, Role: selected.Role, Weight: selected.Weight, RecentCount: selected.RecentCount, Window: 1000}, nil
}
