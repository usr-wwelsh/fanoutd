package llm

import (
	"fmt"
	"sort"
	"strings"
)

// Preset is a known provider: where it answers, and what it needs before it
// will. Adding a vendor is a row here, not code, because the wire protocol is
// already shared — which is the whole reason the list can be this long.
//
// A base URL is the vendor's published OpenAI-compatible endpoint. Local
// servers carry their upstream default port; anything else is what Custom and
// an explicit base URL are for.
type Preset struct {
	Name    string
	BaseURL string
	// KeyOptional marks an endpoint that authenticates nothing, which is the
	// normal case for a server on localhost. A key is still sent if one is set,
	// since a local server sitting behind a proxy may want it.
	KeyOptional bool
	// DefaultModel is only set where the provider has one obvious answer. Empty
	// means the operator has to name a model, which is honest: there is no
	// defensible default among a vendor's whole line-up.
	DefaultModel string
}

// presets are looked up by name, case-insensitively.
var presets = map[string]Preset{
	"openrouter": {
		Name:    "openrouter",
		BaseURL: "https://openrouter.ai/api/v1",
		// The one default worth keeping: it is free, it calls tools, and it is
		// what every existing install is already pointed at.
		DefaultModel: "inclusionai/ling-3.0-flash:free",
	},
	"openai":    {Name: "openai", BaseURL: "https://api.openai.com/v1"},
	"anthropic": {Name: "anthropic", BaseURL: "https://api.anthropic.com/v1"},
	"gemini":    {Name: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai"},
	"groq":      {Name: "groq", BaseURL: "https://api.groq.com/openai/v1"},
	"deepseek":  {Name: "deepseek", BaseURL: "https://api.deepseek.com/v1"},
	"mistral":   {Name: "mistral", BaseURL: "https://api.mistral.ai/v1"},
	"xai":       {Name: "xai", BaseURL: "https://api.x.ai/v1"},
	"together":  {Name: "together", BaseURL: "https://api.together.xyz/v1"},
	"fireworks": {Name: "fireworks", BaseURL: "https://api.fireworks.ai/inference/v1"},
	"cerebras":  {Name: "cerebras", BaseURL: "https://api.cerebras.ai/v1"},

	// Local servers. Not a lesser case: a board that plans and reviews against
	// a model on the same machine is the only configuration that owes nobody a
	// network, which is the point of running one.
	"ollama":   {Name: "ollama", BaseURL: "http://localhost:11434/v1", KeyOptional: true},
	"llamacpp": {Name: "llamacpp", BaseURL: "http://localhost:8080/v1", KeyOptional: true},
	"vllm":     {Name: "vllm", BaseURL: "http://localhost:8000/v1", KeyOptional: true},
	"lmstudio": {Name: "lmstudio", BaseURL: "http://localhost:1234/v1", KeyOptional: true},

	// Custom has no endpoint of its own, so a base URL is required rather than
	// merely allowed. It is how anything not listed above is reached, including
	// a self-hosted server on a port these defaults do not guess.
	"custom": {Name: "custom", KeyOptional: true},
}

// PresetNames lists the known providers in a stable order, for an error message
// or a picker.
func PresetNames() []string {
	names := make([]string, 0, len(presets))
	for name := range presets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// LookupPreset finds a provider by name.
func LookupPreset(name string) (Preset, bool) {
	p, ok := presets[strings.ToLower(strings.TrimSpace(name))]
	return p, ok
}

// Resolve builds the client for a provider, applying the preset's endpoint and
// default model where the operator did not override them.
//
// It fails closed. An unusable provider is a startup error naming what is
// missing, not a client that discovers it one HTTP call into the first run —
// by which point the failure is recorded on a task as if the model had refused.
func Resolve(name, baseURL, apiKey, model string) (*Client, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = "openrouter"
	}
	preset, ok := LookupPreset(name)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q: one of %s", name, strings.Join(PresetNames(), ", "))
	}

	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = preset.BaseURL
	}
	if baseURL == "" {
		return nil, fmt.Errorf("provider %q has no endpoint of its own: set FANOUT_BASE_URL", name)
	}

	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" && !preset.KeyOptional {
		return nil, fmt.Errorf("provider %q needs a key: set FANOUT_API_KEY", name)
	}

	model = strings.TrimSpace(model)
	if model == "" {
		model = preset.DefaultModel
	}
	if model == "" {
		return nil, fmt.Errorf("provider %q has no default model: set FANOUT_MODEL", name)
	}

	return NewClient(preset, apiKey, model, baseURL), nil
}
