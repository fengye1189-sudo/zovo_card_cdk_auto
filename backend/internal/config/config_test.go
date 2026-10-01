package config

import "testing"

func TestLoadRejectsMissingJWTSecretInRelease(t *testing.T) {
	t.Setenv("SERVER_MODE", "release")
	t.Setenv("JWT_SECRET", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected release configuration without JWT_SECRET to fail closed")
	}
}

func TestLoadAcceptsStrongJWTSecretInRelease(t *testing.T) {
	t.Setenv("SERVER_MODE", "release")
	t.Setenv("JWT_SECRET", "01234567890123456789012345678901")
	t.Setenv("JWT_EXPIRATION_HOURS", "12")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected valid release configuration, got %v", err)
	}
	if cfg.JWT.Secret == "" || cfg.JWT.ExpirationHs != 12 {
		t.Fatalf("unexpected jwt config: %+v", cfg.JWT)
	}
}

func TestLoadKeepsDebugCompatibilityDefaults(t *testing.T) {
	t.Setenv("SERVER_MODE", "debug")
	t.Setenv("JWT_SECRET", "")
	if _, err := Load(); err != nil {
		t.Fatalf("debug configuration should remain usable for local setup: %v", err)
	}
}
