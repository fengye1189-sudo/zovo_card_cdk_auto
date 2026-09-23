package server

import "testing"

func TestAdminRoutePermissionMatrix(t *testing.T) {
	tests := []struct {
		role, method, route string
		allowed             bool
	}{
		{"owner", "PUT", "/api/v1/admin/local-cdk-settings", true},
		{"operator", "PUT", "/api/v1/admin/local-cdk-settings", false},
		{"viewer", "GET", "/api/v1/admin/settings", false},
		{"operator", "GET", "/api/v1/admin/automation/settings", false},
		{"operator", "POST", "/api/v1/admin/automation/finance/review", false},
		{"viewer", "GET", "/api/v1/admin/operations/records", true},
		{"viewer", "POST", "/api/v1/admin/operations/customers/search", true},
		{"viewer", "POST", "/api/v1/admin/local-cdks/search", true},
		{"operator", "GET", "/api/v1/admin/local-cdks/reserve", true},
		{"viewer", "POST", "/api/v1/admin/local-cdks/:id/disable", false},
		{"viewer", "GET", "/api/v1/admin/operations/records/export", true},
		{"viewer", "GET", "/api/v1/admin/operations/records/:id/support", true},
		{"viewer", "POST", "/api/v1/admin/operations/records/:id/support", false},
		{"operator", "POST", "/api/v1/admin/operations/records/:id/support", true},
		{"operator", "POST", "/api/v1/admin/local-cdks", true},
		{"viewer", "POST", "/api/v1/admin/local-cdks", false},
		{"operator", "POST", "/api/v1/admin/cardplatform/cdks", false},
		{"operator", "GET", "/api/v1/admin/direct-orders/:id", false},
		{"viewer", "GET", "/api/v1/admin/operations/team", false},
		{"operator", "GET", "/api/v1/admin/operations/backups", false},
		{"operator", "GET", "/api/v1/admin/operations/api-tokens", false},
		{"viewer", "GET", "/api/v1/admin/operations/products", true},
		{"viewer", "GET", "/api/v1/admin/operations/reply-templates", true},
		{"operator", "PUT", "/api/v1/admin/operations/reply-templates", false},
		{"operator", "PUT", "/api/v1/admin/operations/products/:id", false},
		{"operator", "GET", "/api/v1/admin/new-secret-route", false},
		{"viewer", "POST", "/api/v1/auth/admin/change-password", true},
		{"", "GET", "/api/v1/admin/operations/records", false},
	}
	for _, tt := range tests {
		t.Run(tt.role+" "+tt.method+" "+tt.route, func(t *testing.T) {
			if got := adminRouteAllowed(tt.role, tt.method, tt.route); got != tt.allowed {
				t.Fatalf("got %t, want %t", got, tt.allowed)
			}
		})
	}
}
