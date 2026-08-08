package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A setting exported into the environment outrules the file, so writing the
// file changes nothing — not now, and not after a restart either. The settings
// page can only say so if the source comes back with the value.
func TestAnExportedSettingReportsItsSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	if err := os.WriteFile(path, []byte("FANOUT_MODEL=from/file\nFANOUT_TOKEN=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FANOUT_ENV_FILE", path)
	t.Setenv("FANOUT_MODEL", "from/env")

	got := Effective([]string{"FANOUT_MODEL", "FANOUT_TOKEN", "FANOUT_SHELL"})

	if got["FANOUT_MODEL"].Value != "from/env" || got["FANOUT_MODEL"].Source != SourceEnv {
		t.Errorf("FANOUT_MODEL = %+v, want from/env out of the environment", got["FANOUT_MODEL"])
	}
	if got["FANOUT_TOKEN"].Value != "from-file" || got["FANOUT_TOKEN"].Source != SourceFile {
		t.Errorf("FANOUT_TOKEN = %+v, want from-file out of the file", got["FANOUT_TOKEN"])
	}
	if got["FANOUT_SHELL"].Source != SourceUnset {
		t.Errorf("FANOUT_SHELL = %+v, want unset", got["FANOUT_SHELL"])
	}
}

// An install that predates the provider setting still names its key and model
// OPENROUTER_*, and the server runs on them. Reporting those as unset would tell
// an operator their key is missing while their board is happily using it — and
// would have the settings page propose a config with no key in it.
func TestASettingInForceUnderItsOldNameIsReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	contents := "OPENROUTER_API_KEY=sk-legacy\nOPENROUTER_MODEL=vendor/legacy\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FANOUT_ENV_FILE", path)

	got := Effective([]string{"FANOUT_API_KEY", "FANOUT_MODEL"})

	if got["FANOUT_API_KEY"].Value != "sk-legacy" {
		t.Errorf("FANOUT_API_KEY = %+v, want the legacy key it is running on", got["FANOUT_API_KEY"])
	}
	// Which name it came from is the difference between "your key is missing"
	// and "your key is here, under a name this page is about to replace".
	if got["FANOUT_API_KEY"].Key != "OPENROUTER_API_KEY" {
		t.Errorf("key = %q, want the name it was actually read from", got["FANOUT_API_KEY"].Key)
	}
	if got["FANOUT_MODEL"].Value != "vendor/legacy" || got["FANOUT_MODEL"].Source != SourceFile {
		t.Errorf("FANOUT_MODEL = %+v", got["FANOUT_MODEL"])
	}
}

// The current name wins wherever both exist, exactly as Load reads them.
func TestTheCurrentNameOutranksTheOldOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	contents := "OPENROUTER_MODEL=vendor/legacy\nFANOUT_MODEL=vendor/current\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FANOUT_ENV_FILE", path)

	got := Effective([]string{"FANOUT_MODEL"})["FANOUT_MODEL"]
	if got.Value != "vendor/current" || got.Key != "FANOUT_MODEL" {
		t.Errorf("FANOUT_MODEL = %+v, want the current name to win", got)
	}
}

// With no file anywhere, settings still have to land somewhere a restart will
// read them back from.
func TestSettingsFileFallsBackToTheConfigDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FANOUT_ENV_FILE", "")
	t.Setenv("XDG_CONFIG_HOME", dir)
	want := filepath.Join(dir, "fanoutd", "env")
	if got := SettingsFile(); got != want {
		t.Errorf("SettingsFile() = %q, want %q", got, want)
	}
}

func TestSettingsFileIsTheOneAlreadyInUse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	if err := os.WriteFile(path, []byte("PORT=8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FANOUT_ENV_FILE", path)

	if got := SettingsFile(); got != path {
		t.Errorf("SettingsFile() = %q, want the file already loaded, %q", got, path)
	}
}
