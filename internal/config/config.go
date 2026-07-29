package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Port            int
	OpenRouterKey   string
	OpenRouterModel string
	DataDir         string
	DatabasePath    string
	OutputDir       string
	BaseURL         string
	// MaxParallel caps how many subtasks of one breakdown run at once.
	MaxParallel int
	// Token gates the API when set. Empty leaves the server open, which is the
	// default for local use.
	Token string
	// EnvFile records which file the settings were read from, or "" for none.
	EnvFile string
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

	cfg := Config{
		OpenRouterKey:   get("OPENROUTER_API_KEY"),
		OpenRouterModel: "inclusionai/ling-3.0-flash:free",
		BaseURL:         get("OPENROUTER_BASE_URL"),
		Token:           get("FANOUT_TOKEN"),
		Port:            8080,
		EnvFile:         envFile,
	}
	if v := get("OPENROUTER_MODEL"); v != "" {
		cfg.OpenRouterModel = v
	}
	if p, err := strconv.Atoi(get("PORT")); err == nil && p > 0 {
		cfg.Port = p
	}
	if n, err := strconv.Atoi(get("FANOUT_MAX_PARALLEL")); err == nil && n > 0 {
		cfg.MaxParallel = n
	}

	cfg.DataDir = dataDir(get("FANOUT_DATA_DIR"))
	cfg.DatabasePath = resolve(cfg.DataDir, get("DATABASE_PATH"), "fanoutd.db")
	cfg.OutputDir = resolve(cfg.DataDir, get("OUTPUT_DIR"), "output")

	return cfg
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
