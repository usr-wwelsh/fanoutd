package config

import "testing"

func TestMaxStepsReadsTheEnvironment(t *testing.T) {
	t.Setenv("FANOUT_MAX_STEPS", "40")
	if got := Load().MaxSteps; got != 40 {
		t.Errorf("MaxSteps = %d, want 40", got)
	}
}

// Left unset the config carries no opinion, so the agent's own default stands
// rather than a zero silently becoming the limit.
func TestMaxStepsUnsetLeavesZero(t *testing.T) {
	t.Setenv("FANOUT_MAX_STEPS", "")
	if got := Load().MaxSteps; got != 0 {
		t.Errorf("MaxSteps = %d with nothing set, want 0", got)
	}
}

func TestMaxStepsIgnoresRubbish(t *testing.T) {
	t.Setenv("FANOUT_MAX_STEPS", "plenty")
	if got := Load().MaxSteps; got != 0 {
		t.Errorf("MaxSteps = %d from an unparseable value, want 0", got)
	}
}

// The OPENROUTER_* names predate there being a choice of provider. An install
// that still sets them must keep working untouched, or the abstraction costs
// every existing user an edit for a feature they did not ask for.
func TestLegacyOpenRouterNamesStillLoad(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-legacy")
	t.Setenv("OPENROUTER_MODEL", "legacy/model")
	t.Setenv("OPENROUTER_BASE_URL", "https://legacy.example/v1")

	cfg := Load()
	if cfg.APIKey != "sk-legacy" {
		t.Errorf("APIKey = %q", cfg.APIKey)
	}
	if cfg.Model != "legacy/model" {
		t.Errorf("Model = %q", cfg.Model)
	}
	if cfg.BaseURL != "https://legacy.example/v1" {
		t.Errorf("BaseURL = %q", cfg.BaseURL)
	}
}

func TestProviderNamesWinOverLegacyOnes(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-legacy")
	t.Setenv("OPENROUTER_MODEL", "legacy/model")
	t.Setenv("FANOUT_API_KEY", "sk-current")
	t.Setenv("FANOUT_MODEL", "current/model")

	cfg := Load()
	if cfg.APIKey != "sk-current" {
		t.Errorf("APIKey = %q, want the FANOUT_ name to win", cfg.APIKey)
	}
	if cfg.Model != "current/model" {
		t.Errorf("Model = %q, want the FANOUT_ name to win", cfg.Model)
	}
}
