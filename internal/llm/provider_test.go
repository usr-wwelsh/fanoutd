package llm

import (
	"strings"
	"testing"
)

func TestResolveDefaultsToOpenRouter(t *testing.T) {
	c, err := Resolve("", "", "sk-test", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.Provider.Name != "openrouter" {
		t.Errorf("provider = %q, want openrouter", c.Provider.Name)
	}
	if c.BaseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("base URL = %q", c.BaseURL)
	}
	if c.Model == "" {
		t.Error("no default model applied")
	}
}

func TestResolveAppliesPresetEndpoint(t *testing.T) {
	c, err := Resolve("groq", "", "sk-test", "llama-3.3-70b-versatile")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.BaseURL != "https://api.groq.com/openai/v1" {
		t.Errorf("base URL = %q, want groq's", c.BaseURL)
	}
}

// An explicit endpoint is how a preset is pointed at something else — a local
// server on a non-default port, or a proxy in front of the vendor.
func TestResolveBaseURLOverridesPreset(t *testing.T) {
	c, err := Resolve("ollama", "http://box.lan:9999/v1/", "", "qwen3")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.BaseURL != "http://box.lan:9999/v1" {
		t.Errorf("base URL = %q, want the override with its slash trimmed", c.BaseURL)
	}
}

// A local server authenticates nothing, so requiring a key would make the
// offline case the awkward one.
func TestResolveLocalProviderNeedsNoKey(t *testing.T) {
	if _, err := Resolve("ollama", "", "", "qwen3"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
}

func TestResolveRejectsIncompleteProvider(t *testing.T) {
	tests := []struct {
		name                          string
		provider, baseURL, key, model string
	}{
		{"unknown provider", "gpt5-direct", "", "sk-test", "m"},
		{"vendor without a key", "groq", "", "", "m"},
		{"vendor without a model", "groq", "", "sk-test", ""},
		{"custom without an endpoint", "custom", "", "sk-test", "m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Resolve(tt.provider, tt.baseURL, tt.key, tt.model); err == nil {
				t.Fatal("resolved a provider that cannot work")
			}
		})
	}
}

// The key is the one field an error must never repeat: startup logs are the
// first thing pasted into a bug report.
func TestResolveErrorsOmitTheKey(t *testing.T) {
	_, err := Resolve("groq", "", "sk-secret-value", "")
	if err == nil {
		t.Fatal("want an error for a missing model")
	}
	if got := err.Error(); strings.Contains(got, "sk-secret-value") {
		t.Errorf("error names the key: %q", got)
	}
}
