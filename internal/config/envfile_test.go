package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritingASettingLeavesCommentsAndOtherKeysAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	original := "# Which endpoint to talk to.\nFANOUT_PROVIDER=openrouter\n\n# The key.\nFANOUT_API_KEY=sk-old\nPORT=8080\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteEnvFile(path, map[string]string{"FANOUT_API_KEY": "sk-new"}, nil); err != nil {
		t.Fatalf("WriteEnvFile: %v", err)
	}

	got := read(t, path)
	want := "# Which endpoint to talk to.\nFANOUT_PROVIDER=openrouter\n\n# The key.\nFANOUT_API_KEY=sk-new\nPORT=8080\n"
	if got != want {
		t.Errorf("file =\n%q\nwant\n%q", got, want)
	}
}

// A settings file made from .env.example is almost all commented-out defaults.
// Those are documentation, not values: uncommenting one in place would edit the
// prose explaining it, so a new setting is appended instead.
func TestASettingWithNoLiveLineIsAppended(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte("# FANOUT_MAX_STEPS=40\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteEnvFile(path, map[string]string{"FANOUT_MAX_STEPS": "60"}, nil); err != nil {
		t.Fatalf("WriteEnvFile: %v", err)
	}

	got := read(t, path)
	if !strings.Contains(got, "# FANOUT_MAX_STEPS=40\n") {
		t.Errorf("the commented default was disturbed:\n%s", got)
	}
	if !strings.Contains(got, "\nFANOUT_MAX_STEPS=60\n") {
		t.Errorf("the new value was not appended:\n%s", got)
	}
	if _, vals := readEnvFile(path); vals["FANOUT_MAX_STEPS"] != "60" {
		t.Errorf("reading back gives %q, want 60", vals["FANOUT_MAX_STEPS"])
	}
}

// The value is attacker-controlled the moment settings are editable over HTTP.
// A newline in it would append a line of its own — FANOUT_SHELL=1 under a
// FANOUT_MODEL the operator typed — so it is refused rather than escaped.
func TestAValueCannotInjectASecondSetting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte("PORT=8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := WriteEnvFile(path, map[string]string{"FANOUT_MODEL": "some/model\nFANOUT_SHELL=1"}, nil)
	if err == nil {
		t.Fatal("a value containing a newline was accepted")
	}
	if got := read(t, path); got != "PORT=8080\n" {
		t.Errorf("the file was touched despite the error:\n%s", got)
	}
}

func TestAKeyOutsideTheAllowedShapeIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	for _, key := range []string{"", "not a key", "KEY=OTHER", "KEY\nOTHER"} {
		if err := WriteEnvFile(path, map[string]string{key: "x"}, nil); err == nil {
			t.Errorf("key %q was accepted", key)
		}
	}
}

// The file holds an API key and the token gating the board, so it must not be
// readable by other accounts on the machine.
func TestTheFileIsWrittenPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	if err := WriteEnvFile(path, map[string]string{"FANOUT_API_KEY": "sk-secret"}, nil); err != nil {
		t.Fatalf("WriteEnvFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

// Setting FANOUT_API_KEY while OPENROUTER_API_KEY is still in the file would
// leave a superseded secret sitting there. Dropping it is what keeps the file
// saying what the server is actually using.
func TestDroppedKeysAreRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte("# legacy\nOPENROUTER_API_KEY=sk-legacy\nPORT=8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := WriteEnvFile(path, map[string]string{"FANOUT_API_KEY": "sk-new"}, []string{"OPENROUTER_API_KEY"})
	if err != nil {
		t.Fatalf("WriteEnvFile: %v", err)
	}

	got := read(t, path)
	if strings.Contains(got, "sk-legacy") {
		t.Errorf("the superseded key survived:\n%s", got)
	}
	if !strings.Contains(got, "PORT=8080") || !strings.Contains(got, "# legacy") {
		t.Errorf("more than the dropped line went:\n%s", got)
	}
}

func TestWritingCreatesTheFileAndItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "fanoutd", "env")
	if err := WriteEnvFile(path, map[string]string{"PORT": "9090"}, nil); err != nil {
		t.Fatalf("WriteEnvFile: %v", err)
	}
	if _, vals := readEnvFile(path); vals["PORT"] != "9090" {
		t.Errorf("PORT = %q, want 9090", vals["PORT"])
	}
}

// A value with spaces or a "#" round-trips: the reader strips one layer of
// quotes, so the writer has to add it back or the setting comes back truncated.
func TestValuesNeedingQuotesSurviveARoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	values := map[string]string{
		"FANOUT_SHELL_CPU":    "200%",
		"FANOUT_MODEL":        "vendor/model # not a comment",
		"FANOUT_SHELL_ROBIND": "/home/me/.cargo:/home/me/.rustup",
		"FANOUT_TOKEN":        "  spaced  ",
	}
	if err := WriteEnvFile(path, values, nil); err != nil {
		t.Fatalf("WriteEnvFile: %v", err)
	}
	_, back := readEnvFile(path)
	for key, want := range values {
		if back[key] != want {
			t.Errorf("%s = %q, want %q", key, back[key], want)
		}
	}
}

// An empty value is a real setting — it is how FANOUT_TOKEN is cleared, which
// takes the gate off the API. Dropping the line instead would silently fall
// back to whatever the next source says.
func TestAnEmptyValueIsWrittenRatherThanSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte("FANOUT_TOKEN=old-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteEnvFile(path, map[string]string{"FANOUT_TOKEN": ""}, nil); err != nil {
		t.Fatalf("WriteEnvFile: %v", err)
	}
	got := read(t, path)
	if strings.Contains(got, "old-token") {
		t.Errorf("the old token survived being cleared:\n%s", got)
	}
	if !strings.Contains(got, "FANOUT_TOKEN=") {
		t.Errorf("the cleared setting lost its line:\n%s", got)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
