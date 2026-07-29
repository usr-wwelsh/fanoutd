package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FANOUT_CONFIG", path)
}

func TestResolveSettingsPrecedence(t *testing.T) {
	writeConfig(t, "url = \"https://file.example\"\ntoken = \"file-token\"\n")

	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("FANOUT_URL", "https://env.example")
		got := resolveSettings("https://flag.example", "")
		if got.server != "https://flag.example" {
			t.Errorf("server = %q", got.server)
		}
		if got.token != "file-token" {
			t.Errorf("token = %q, want the file value when no flag or env is set", got.token)
		}
	})

	t.Run("env beats file", func(t *testing.T) {
		t.Setenv("FANOUT_URL", "https://env.example")
		t.Setenv("FANOUT_TOKEN", "env-token")
		got := resolveSettings("", "")
		if got.server != "https://env.example" || got.token != "env-token" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("file beats the default", func(t *testing.T) {
		os.Unsetenv("FANOUT_URL")
		os.Unsetenv("FANOUT_TOKEN")
		got := resolveSettings("", "")
		if got.server != "https://file.example" {
			t.Errorf("server = %q", got.server)
		}
	})
}

func TestResolveSettingsDefault(t *testing.T) {
	t.Setenv("FANOUT_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))
	os.Unsetenv("FANOUT_URL")
	os.Unsetenv("FANOUT_TOKEN")

	got := resolveSettings("", "")
	if got.server != defaultServer {
		t.Errorf("server = %q, want %q", got.server, defaultServer)
	}
	if got.token != "" {
		t.Errorf("token = %q, want empty", got.token)
	}
	if got.file != "" {
		t.Errorf("file = %q, want empty when no config exists", got.file)
	}
}

// A bare host is the natural thing to type. Turning it into a URL here beats
// an unhelpful "unsupported protocol scheme" from net/http.
func TestResolveSettingsNormalizesServer(t *testing.T) {
	t.Setenv("FANOUT_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))
	os.Unsetenv("FANOUT_URL")

	tests := map[string]string{
		"board.example:8080":        "http://board.example:8080",
		"https://board.example/":    "https://board.example",
		"http://localhost:8080///":  "http://localhost:8080",
		"https://board.example/api": "https://board.example/api",
	}
	for in, want := range tests {
		if got := resolveSettings(in, "").server; got != want {
			t.Errorf("resolveSettings(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadConfigFile(t *testing.T) {
	writeConfig(t, `
# a comment
[server]
url = "https://board.example"   # trailing comment on a quoted value
token = plain-token   # unquoted values work too

junk line with no equals
`)
	path, vals := loadConfigFile()
	if path == "" {
		t.Fatal("expected the config path to be reported")
	}
	if vals["url"] != "https://board.example" {
		t.Errorf("url = %q", vals["url"])
	}
	if vals["token"] != "plain-token" {
		t.Errorf("token = %q", vals["token"])
	}
}
