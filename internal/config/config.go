package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port int
	// Provider names which endpoint to talk to — a preset in internal/llm, or
	// "custom" with an explicit BaseURL. Everything else about a vendor is that
	// one row, because they all speak the same wire protocol.
	Provider string
	APIKey   string
	Model    string
	// BaseURL overrides the provider's published endpoint. It is what points
	// fanoutd at a local server on a port the presets do not guess.
	BaseURL      string
	DataDir      string
	DatabasePath string
	OutputDir    string
	// MaxParallel caps how many subtasks of one breakdown run at once.
	MaxParallel int
	// MaxSteps bounds one agent run. Zero leaves the agent's own default, which
	// is the only sane reading of an unset value: a budget of nothing would
	// concede every task on its first step.
	MaxSteps int
	// Token gates the API when set. Empty leaves the server open, which is the
	// default for local use.
	Token string
	// EnvFile records which file the settings were read from, or "" for none.
	EnvFile string
	// Review sends a finished run to a second agent before it lands in the
	// finished column. Off by default: it spends a second model call per task and
	// can create rework tasks of its own, which is not something to switch on
	// behind an operator's back.
	Review bool
	// ReviewModel is the model the reviewer runs on. Empty means the same one the
	// task used — honest, and much weaker: a model reviewing its own output
	// agrees with it. It must support tool calls.
	ReviewModel string
	// Shell enables the sandboxed run_command tool. Off by default, and ignored
	// when bubblewrap will not run.
	Shell        bool
	ShellNet     bool
	ShellTimeout time.Duration
	ShellMemory  string
	ShellTasks   int
	ShellCPU     string
	ShellMaxExec int
	ShellROBind  []string
	SandboxDir   string
}

// Load reads defaults, then an env file, then the environment. Exported
// variables win over the file.
//
// Nothing resolves against the working directory: a relative DATABASE_PATH or
// OUTPUT_DIR is taken relative to the data directory, so the server behaves the
// same whether it is started from the repo, from a systemd unit, or from /.
func Load() Config {
	envFile, fileVals := loadEnvFile()

	get := func(key string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fileVals[key]
	}

	// The OPENROUTER_* names are the originals, from when there was only one
	// provider. They still work and mean the same thing, so an existing env file
	// keeps working untouched; the FANOUT_* names are what the settings describe
	// now that the endpoint is a choice.
	either := func(preferred, legacy string) string {
		if v := get(preferred); v != "" {
			return v
		}
		return get(legacy)
	}

	cfg := Config{
		Provider: get("FANOUT_PROVIDER"),
		APIKey:   either("FANOUT_API_KEY", "OPENROUTER_API_KEY"),
		Model:    either("FANOUT_MODEL", "OPENROUTER_MODEL"),
		BaseURL:  either("FANOUT_BASE_URL", "OPENROUTER_BASE_URL"),
		Token:    get("FANOUT_TOKEN"),
		Port:     8080,
		EnvFile:  envFile,
	}
	if p, err := strconv.Atoi(get("PORT")); err == nil && p > 0 {
		cfg.Port = p
	}
	if n, err := strconv.Atoi(get("FANOUT_MAX_PARALLEL")); err == nil && n > 0 {
		cfg.MaxParallel = n
	}
	if n, err := strconv.Atoi(get("FANOUT_MAX_STEPS")); err == nil && n > 0 {
		cfg.MaxSteps = n
	}

	cfg.Review = truthy(get("FANOUT_REVIEW"))
	cfg.ReviewModel = strings.TrimSpace(get("FANOUT_REVIEW_MODEL"))

	cfg.Shell = truthy(get("FANOUT_SHELL"))
	cfg.ShellNet = truthy(get("FANOUT_SHELL_NET"))
	cfg.ShellMemory = "2G"
	cfg.ShellTasks = 512
	cfg.ShellCPU = "200%"
	cfg.ShellTimeout = 120 * time.Second
	if v := get("FANOUT_SHELL_MEMORY"); v != "" {
		cfg.ShellMemory = v
	}
	if v := get("FANOUT_SHELL_CPU"); v != "" {
		cfg.ShellCPU = v
	}
	if n, err := strconv.Atoi(get("FANOUT_SHELL_TASKS")); err == nil && n > 0 {
		cfg.ShellTasks = n
	}
	if n, err := strconv.Atoi(get("FANOUT_SHELL_TIMEOUT")); err == nil && n > 0 {
		cfg.ShellTimeout = time.Duration(n) * time.Second
	}
	if n, err := strconv.Atoi(get("FANOUT_MAX_EXEC")); err == nil && n > 0 {
		cfg.ShellMaxExec = n
	}
	cfg.ShellROBind = splitPaths(get("FANOUT_SHELL_ROBIND"))

	cfg.DataDir = dataDir(get("FANOUT_DATA_DIR"))
	cfg.DatabasePath = resolve(cfg.DataDir, get("DATABASE_PATH"), "fanoutd.db")
	cfg.OutputDir = resolve(cfg.DataDir, get("OUTPUT_DIR"), "output")
	cfg.SandboxDir = resolve(cfg.DataDir, get("FANOUT_SANDBOX_DIR"), "sandbox")

	return cfg
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// splitPaths reads a colon-separated path list, PATH-style.
func splitPaths(v string) []string {
	paths := []string{}
	for _, p := range strings.Split(v, ":") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, abs(p))
		}
	}
	return paths
}

// dataDir picks where state lives: an explicit setting, else XDG, else the
// working directory as a last resort when there is no home to speak of.
func dataDir(explicit string) string {
	if explicit != "" {
		return abs(explicit)
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(abs(xdg), "fanoutd")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "fanoutd")
	}
	return abs("fanoutd-data")
}

// resolve treats a relative path as relative to the data directory rather than
// the working directory. Absolute paths are taken as given.
func resolve(dataDir, value, fallback string) string {
	if value == "" {
		value = fallback
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(dataDir, value)
}

func abs(path string) string {
	if a, err := filepath.Abs(path); err == nil {
		return a
	}
	return path
}

// envFilePath returns the first env file that exists: an explicit override, the
// working directory (the development case), then the XDG config directory (the
// deployed case).
func envFilePath() string {
	if v := os.Getenv("FANOUT_ENV_FILE"); v != "" {
		return v
	}
	candidates := []string{".env"}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "fanoutd", "env"))
	} else if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "fanoutd", "env"))
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

func loadEnvFile() (string, map[string]string) {
	vals := map[string]string{}
	path := envFilePath()
	if path == "" {
		return "", vals
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", vals
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		vals[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(val), "\"'")
	}
	return path, vals
}
