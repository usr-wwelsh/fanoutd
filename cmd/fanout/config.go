package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

const defaultServer = "http://localhost:8080"

// settings is the resolved server address and token, in precedence order:
// flag, then environment, then config file, then the localhost default.
//
// Nothing is derived from the working directory or the binary's location. A
// laptop points at a remote board once and every invocation reaches it.
type settings struct {
	server string
	token  string
	// file records which config file supplied values, or "" for none.
	file string
}

func resolveSettings(flagServer, flagToken string) settings {
	file, vals := loadConfigFile()
	s := settings{file: file}

	s.server = firstNonEmpty(flagServer, os.Getenv("FANOUT_URL"), vals["url"], defaultServer)
	s.token = firstNonEmpty(flagToken, os.Getenv("FANOUT_TOKEN"), vals["token"])

	if !strings.Contains(s.server, "://") {
		s.server = "http://" + s.server
	}
	s.server = strings.TrimRight(s.server, "/")
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func configPath() string {
	if v := os.Getenv("FANOUT_CONFIG"); v != "" {
		return v
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "fanoutd", "config.toml")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "fanoutd", "config.toml")
	}
	return ""
}

// loadConfigFile reads the flat `key = "value"` subset of TOML that this config
// actually uses. A real TOML parser would be a dependency for two keys.
func loadConfigFile() (string, map[string]string) {
	vals := map[string]string{}
	path := configPath()
	if path == "" {
		return "", vals
	}
	f, err := os.Open(path)
	if err != nil {
		return "", vals
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		vals[strings.ToLower(strings.TrimSpace(key))] = parseValue(val)
	}
	return path, vals
}

// parseValue unwraps a quoted string, or strips a trailing comment from a bare
// one. A '#' inside quotes is part of the value — tokens contain punctuation.
func parseValue(val string) string {
	val = strings.TrimSpace(val)
	if len(val) > 0 && (val[0] == '"' || val[0] == '\'') {
		if end := strings.IndexByte(val[1:], val[0]); end >= 0 {
			return val[1 : end+1]
		}
		return val[1:]
	}
	if i := strings.IndexByte(val, '#'); i >= 0 {
		val = val[:i]
	}
	return strings.TrimSpace(val)
}
