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
