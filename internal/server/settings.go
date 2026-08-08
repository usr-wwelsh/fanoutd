package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"fanoutd/internal/agent"
	"fanoutd/internal/config"
	"fanoutd/internal/llm"
)

// The settings endpoint edits the same file the server reads at startup, and
// then makes the change true of the running process. Two rules keep that from
// being a hole rather than a feature:
//
//   - Secrets go in and never come back out. Before this existed, the API key
//     lived in a file and a process and was reachable over HTTP from neither.
//     Serving it back — even to a caller holding the token — would make every
//     board a place a key can be read out of, so a secret answers with whether
//     it is set and nothing else.
//   - Keys are an allowlist and values are validated per kind. The file has no
//     escaping, so a value carrying a newline would write a setting of its own;
//     a key nobody vetted would write any variable the process later reads.
//
// Everything else is applied live. What cannot be — the port the listener is
// bound to, and the paths the database and workspaces were opened from — is
// saved and reported, because pretending otherwise is worse than a banner.

type settingKind string

const (
	kindText   settingKind = "text"
	kindSecret settingKind = "secret"
	kindInt    settingKind = "int"
	kindBool   settingKind = "bool"
	kindEnum   settingKind = "enum"
	// kindPaths is a colon-separated path list, PATH-style.
	kindPaths settingKind = "paths"
)

// settingField is one editable setting. The list is the single description of
// what the settings are: the form is rendered from it, the request is validated
// against it, and the file is written from it, so adding a setting is a row here
// rather than an edit in three places that can disagree.
type settingField struct {
	Key   string      `json:"key"`
	Label string      `json:"label"`
	Help  string      `json:"help"`
	Kind  settingKind `json:"kind"`
	Group string      `json:"group"`
	// Choices is the allowlist for kindEnum, filled in at serve time for the
	// provider list so it cannot drift from the presets it names.
	Choices []string `json:"choices,omitempty"`
	// Placeholder is what the field shows when it is empty, which for most of
	// these is the default that applies when nothing is set.
	Placeholder string `json:"placeholder,omitempty"`
	// Restart marks a setting the running process cannot adopt.
	Restart bool `json:"restart,omitempty"`
	// Half asks for a field half the form's width, so a pair that is read
	// together is laid out together. The two model fields are the case: the
	// reviewer model is only meaningful against the model it is judging.
	Half bool `json:"half,omitempty"`
}

const (
	grpProvider = "Provider"
	grpModels   = "Models"
	grpAgent    = "Agent"
	grpShell    = "Shell"
	grpServer   = "Server"
	grpStorage  = "Storage"
)

var settingFields = []settingField{
	{
		Key: "FANOUT_PROVIDER", Label: "Provider", Kind: kindEnum, Group: grpProvider,
		Placeholder: "openrouter",
		Help:        "Which endpoint to talk to. They all speak the same protocol, so switching is one field.",
	},
	{
		Key: "FANOUT_API_KEY", Label: "API key", Kind: kindSecret, Group: grpProvider,
		Help: "Not needed for a local server. Stored in the settings file, which is written private to you.",
	},
	{
		Key: "FANOUT_BASE_URL", Label: "Base URL", Kind: kindText, Group: grpProvider,
		Help: "Overrides where the provider answers — a local server on an unusual port, or a proxy. Required for Custom.",
	},

	// The two models sit side by side because the second only means anything
	// against the first: a reviewer left empty runs on whatever the task used,
	// and a model reviewing its own output agrees with it. Seeing one number
	// next to the other is what makes that visible.
	{
		Key: "FANOUT_MODEL", Label: "Default model", Kind: kindText, Group: grpModels, Half: true,
		Help: "What a task with no model of its own runs on.",
	},
	{
		Key: "FANOUT_REVIEW_MODEL", Label: "Reviewer model", Kind: kindText, Group: grpModels, Half: true,
		Placeholder: "the task's own model",
		Help:        "Pick a different one that supports tool calls.",
	},
	{
		Key: "FANOUT_REVIEW", Label: "Review finished work", Kind: kindBool, Group: grpModels,
		Help: "Sends every finished run to a second agent, which either passes it or sends it back as a rework task. Costs a model call per task.",
	},

	{
		Key: "FANOUT_MAX_PARALLEL", Label: "Parallel subtasks", Kind: kindInt, Group: grpAgent,
		Placeholder: "3",
		Help:        "How many subtasks of one breakdown run at once. The limit that bites first is the provider's, not the machine's.",
	},
	{
		Key: "FANOUT_MAX_STEPS", Label: "Steps per run", Kind: kindInt, Group: grpAgent,
		Placeholder: "40",
		Help:        "A budget, not a safety rail — the agent already stops itself on repetition. Too low and a run concedes three steps from working code.",
	},

	{
		Key: "FANOUT_SHELL", Label: "Shell commands", Kind: kindBool, Group: grpShell,
		Help: "Lets agents build and test what they write, inside bubblewrap. Ignored where the sandbox will not start.",
	},
	{
		Key: "FANOUT_SHELL_NET", Label: "Network in the sandbox", Kind: kindBool, Group: grpShell,
		Help: "Off means no dependency downloads — and no way to exfiltrate a workspace or your API key.",
	},
	{
		Key: "FANOUT_SHELL_TIMEOUT", Label: "Command timeout (seconds)", Kind: kindInt, Group: grpShell,
		Placeholder: "120",
	},
	{Key: "FANOUT_SHELL_MEMORY", Label: "Memory limit", Kind: kindText, Group: grpShell, Placeholder: "2G"},
	{Key: "FANOUT_SHELL_CPU", Label: "CPU quota", Kind: kindText, Group: grpShell, Placeholder: "200%"},
	{Key: "FANOUT_SHELL_TASKS", Label: "Task limit", Kind: kindInt, Group: grpShell, Placeholder: "512"},
	{
		Key: "FANOUT_MAX_EXEC", Label: "Concurrent commands", Kind: kindInt, Group: grpShell,
		Placeholder: "unlimited",
		Help:        "Across all tasks. The cgroup limits already bound the machine, so set this only if you see thrash.",
	},
	{
		Key: "FANOUT_SHELL_ROBIND", Label: "Extra read-only mounts", Kind: kindPaths, Group: grpShell,
		Placeholder: "/home/you/.cargo:/home/you/.rustup",
		Help:        "Colon-separated host paths, for toolchains outside /usr. Every agent can read whatever you bind here — a home directory would include your ssh keys.",
	},

	{
		Key: "FANOUT_TOKEN", Label: "Access token", Kind: kindSecret, Group: grpServer,
		Help: "Gates the API and the served workspaces. Empty leaves the board open, which is fine on localhost and nowhere else. Changing it signs out every browser, including this one.",
	},
	{
		Key: "PORT", Label: "Port", Kind: kindInt, Group: grpServer, Placeholder: "8080", Restart: true,
		Help: "The listener is already bound, so this one waits for a restart.",
	},

	{
		Key: "FANOUT_DATA_DIR", Label: "Data directory", Kind: kindText, Group: grpStorage, Restart: true,
		Placeholder: "~/.local/share/fanoutd",
		Help:        "Where the database and workspaces live. Relative paths below are resolved inside it.",
	},
	{
		Key: "DATABASE_PATH", Label: "Database", Kind: kindText, Group: grpStorage, Restart: true,
		Placeholder: "fanoutd.db",
	},
	{
		Key: "OUTPUT_DIR", Label: "Workspaces", Kind: kindText, Group: grpStorage, Restart: true,
		Placeholder: "output",
	},
	{
		Key: "FANOUT_SANDBOX_DIR", Label: "Sandbox scratch", Kind: kindText, Group: grpStorage,
		Placeholder: "sandbox",
		Help:        "Shared toolchain caches and per-task build directories.",
	},
}

// settingByKey is the allowlist a request is checked against.
var settingByKey = func() map[string]settingField {
	m := make(map[string]settingField, len(settingFields))
	for _, f := range settingFields {
		m[f.Key] = f
	}
	return m
}()

func settingKeys() []string {
	keys := make([]string, 0, len(settingFields))
	for _, f := range settingFields {
		keys = append(keys, f.Key)
	}
	return keys
}

// settingValue is one field as served: its definition, what it is currently set
// to, and where that came from. A secret carries no value at all.
type settingValue struct {
	settingField
	Value  string `json:"value"`
	Source string `json:"source"`
	Set    bool   `json:"set"`
	// Hint identifies which secret is loaded without disclosing it. Only the
	// API key gets one: the token is the credential to this board, and showing
	// any part of it shortens a guess at the rest.
	Hint string `json:"hint,omitempty"`
	// LegacyKey is set when the value in force arrived under a name this
	// setting replaced. Without it a board running on OPENROUTER_API_KEY reads
	// as having no key at all, which is alarming and wrong.
	LegacyKey string `json:"legacy_key,omitempty"`
}

type settingsView struct {
	// File is where a save is written, which is not always where the current
	// values came from: with no settings file anywhere, this is the one that
	// would be created.
	File           string         `json:"file"`
	Fields         []settingValue `json:"fields"`
	RestartPending []string       `json:"restart_pending"`
	Warnings       []string       `json:"warnings"`
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeSettings(w, nil)
	case http.MethodPut:
		s.saveSettings(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) writeSettings(w http.ResponseWriter, warnings []string) {
	current := config.Effective(settingKeys())
	view := settingsView{
		File:           config.SettingsFile(),
		Fields:         make([]settingValue, 0, len(settingFields)),
		RestartPending: s.restartPending(),
		Warnings:       warnings,
	}
	if view.Warnings == nil {
		view.Warnings = []string{}
	}

	// What an empty field would actually fall back to, which is the provider's
	// business and not a constant: leaving the model blank on OpenRouter is a
	// real model, and on OpenAI it is a startup error.
	preset, _ := llm.LookupPreset(defaultString(current["FANOUT_PROVIDER"].Value, "openrouter"))

	for _, f := range settingFields {
		switch f.Key {
		case "FANOUT_PROVIDER":
			f.Choices = llm.PresetNames()
		case "FANOUT_MODEL":
			f.Placeholder = defaultString(preset.DefaultModel, "required — this provider has no default")
		case "FANOUT_BASE_URL":
			f.Placeholder = defaultString(preset.BaseURL, "required for a custom provider")
		}
		set := current[f.Key]
		out := settingValue{settingField: f, Source: string(set.Source), Set: set.Value != ""}
		if set.Key != "" && set.Key != f.Key {
			out.LegacyKey = set.Key
		}
		if f.Kind == kindSecret {
			if f.Key == "FANOUT_API_KEY" {
				out.Hint = maskSecret(set.Value)
			}
		} else {
			out.Value = set.Value
		}
		view.Fields = append(view.Fields, out)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(view)
}

func defaultString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// maskSecret shows just enough to tell one key from another. Fewer than eight
// characters is not a key worth hinting at, and showing most of a short string
// is showing the string.
func maskSecret(value string) string {
	if len(value) < 8 {
		return ""
	}
	return "…" + value[len(value)-4:]
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		// Values carries only the settings being changed. A key left out is
		// left alone, which is what lets the form save without asking the
		// operator to re-type their API key every time.
		Values map[string]string `json:"values"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	updates, err := validateSettings(req.Values)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// The provider is resolved against what the settings would become, before
	// anything is written. A rejected form leaves the file, the client and the
	// running loop exactly as they were.
	proposed := config.Effective(settingKeys())
	merged := make(map[string]string, len(proposed)+len(updates))
	for key, set := range proposed {
		merged[key] = set.Value
	}
	for key, value := range updates {
		merged[key] = value
	}
	if _, err := llm.Resolve(config.FromValues(merged).Provider, merged["FANOUT_BASE_URL"], merged["FANOUT_API_KEY"], merged["FANOUT_MODEL"]); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	drop := []string{}
	for key := range updates {
		// Writing the current name retires the one it replaced, so the file does
		// not end up holding two API keys with only one of them in use.
		drop = append(drop, config.LegacyNames(key)...)
	}
	if err := config.WriteEnvFile(config.SettingsFile(), updates, drop); err != nil {
		http.Error(w, "could not write the settings file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Reloaded rather than assumed: an exported variable still outrules the
	// file, so this is the only way to report what is actually in force.
	warnings := s.apply(config.Load())
	s.writeSettings(w, warnings)
}

// validateSettings checks every key against the allowlist and every value
// against its kind, and returns what should be written. It normalises as it
// goes, so the file holds "1" and "0" rather than whatever spelling of yes the
// form happened to send.
func validateSettings(values map[string]string) (map[string]string, error) {
	if len(values) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(values))
	for _, key := range sortedKeys(values) {
		field, ok := settingByKey[key]
		if !ok {
			return nil, fmt.Errorf("%q is not a setting", key)
		}
		value := strings.TrimSpace(values[key])
		if strings.ContainsAny(value, "\n\r") {
			return nil, fmt.Errorf("%s cannot contain a line break", field.Label)
		}

		switch field.Kind {
		case kindInt:
			if value != "" {
				n, err := strconv.Atoi(value)
				if err != nil || n < 0 {
					return nil, fmt.Errorf("%s must be a whole number, not %q", field.Label, value)
				}
				value = strconv.Itoa(n)
			}
		case kindBool:
			value = boolValue(value)
		case kindEnum:
			if value != "" {
				if _, ok := llm.LookupPreset(value); !ok {
					return nil, fmt.Errorf("%q is not a provider: one of %s", value, strings.Join(llm.PresetNames(), ", "))
				}
				value = strings.ToLower(value)
			}
		case kindText:
			if key == "FANOUT_BASE_URL" && value != "" {
				if err := checkEndpoint(value); err != nil {
					return nil, err
				}
			}
		}
		out[key] = value
	}
	return out, nil
}

// checkEndpoint holds the base URL to something the HTTP client can actually
// reach. It is an allowlist of schemes rather than a check for the obviously
// wrong ones: a file:// or unix:// "endpoint" is a request to read something
// that is not a model server.
func checkEndpoint(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("the base URL must be an http:// or https:// address, not %q", value)
	}
	return nil
}

func boolValue(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return "1"
	}
	return "0"
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// apply makes a config true of the running process, and returns what it could
// not do. Everything here is deliberately idempotent: apply is called with the
// whole config rather than a diff, so a setting that did not change is simply
// set to what it already was.
func (s *Server) apply(cfg config.Config) []string {
	var warnings []string

	// The settings the process is bound to keep the values it is bound to, so
	// the running config keeps describing the running server. Overwriting them
	// with what the file now says would leave a board reporting a port it is not
	// listening on and a database it has not opened — and would erase the very
	// difference restartPending exists to notice.
	running := s.config()
	// Sandbox scratch is resolved inside the data directory, so it moves only
	// once the data directory the rest of the state lives in has moved too.
	if cfg.DataDir != running.DataDir {
		cfg.SandboxDir = running.SandboxDir
	}
	cfg.Port = running.Port
	cfg.DataDir = running.DataDir
	cfg.DatabasePath = running.DatabasePath
	cfg.OutputDir = running.OutputDir

	client, err := llm.Resolve(cfg.Provider, cfg.BaseURL, cfg.APIKey, cfg.Model)
	if err != nil {
		// Validation already resolved the proposed settings, so reaching here
		// means an exported variable overrode what was saved. Keeping the old
		// client is the only choice that leaves the board working.
		warnings = append(warnings, "the provider was left as it was: "+err.Error())
	} else {
		s.loop.SetClient(client)
	}

	s.loop.SetMaxParallel(cfg.MaxParallel)
	s.loop.SetMaxSteps(cfg.MaxSteps)
	s.loop.SetReview(cfg.Review, cfg.ReviewModel)

	if w := s.applySandbox(cfg); w != "" {
		warnings = append(warnings, w)
	}

	s.mu.Lock()
	s.cfg = cfg
	if client != nil {
		s.client = client
	}
	s.mu.Unlock()

	return warnings
}

// applySandbox rebuilds the jail, or takes it away. A sandbox that will not
// start withholds run_command rather than degrading into an unsandboxed shell,
// which is the same rule startup follows — the difference is that here there is
// somebody at a screen to tell.
func (s *Server) applySandbox(cfg config.Config) string {
	if !cfg.Shell {
		s.loop.SetSandbox(nil)
		return ""
	}
	sb, err := agent.NewSandbox(agent.SandboxConfig{
		Network:   cfg.ShellNet,
		Timeout:   cfg.ShellTimeout,
		MemoryMax: cfg.ShellMemory,
		TasksMax:  cfg.ShellTasks,
		CPUQuota:  cfg.ShellCPU,
		MaxExec:   cfg.ShellMaxExec,
		ROBind:    cfg.ShellROBind,
		StateDir:  cfg.SandboxDir,
	})
	if err != nil {
		s.loop.SetSandbox(nil)
		log.Printf("shell commands disabled: %v\n", err)
		return "shell commands are off: " + err.Error()
	}
	s.loop.SetSandbox(sb)
	log.Printf("shell commands enabled (%s)\n", sb.Describe())
	return ""
}

// restartPending names the settings the file now says and the process does not.
// These are the four the running server cannot adopt: the listener is bound, and
// the database and workspace directories were opened at startup.
func (s *Server) restartPending() []string {
	running := s.config()
	saved := config.Load()

	var pending []string
	for _, c := range []struct {
		key           string
		running, disk string
	}{
		{"PORT", strconv.Itoa(running.Port), strconv.Itoa(saved.Port)},
		{"FANOUT_DATA_DIR", running.DataDir, saved.DataDir},
		{"DATABASE_PATH", running.DatabasePath, saved.DatabasePath},
		{"OUTPUT_DIR", running.OutputDir, saved.OutputDir},
	} {
		if c.running != c.disk {
			pending = append(pending, c.key)
		}
	}
	if pending == nil {
		return []string{}
	}
	return pending
}
