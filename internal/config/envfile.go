package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// keyShape is what a settings key may look like. It is an allowlist rather than
// a list of forbidden characters because the file format has no escaping: any
// key that reached the file with an "=" or a newline in it would write a line
// that reads back as something else entirely.
var keyShape = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// WriteEnvFile updates settings in place and leaves everything else as it was.
//
// The file is documentation as much as configuration — .env.example is mostly
// prose explaining commented-out defaults — so comments, blank lines, ordering
// and unrecognised keys all survive. Only a live "KEY=value" line is rewritten;
// a setting with no live line is appended, which leaves the commented default
// standing above it as the note it was written to be.
//
// It is atomic and private: the file holds an API key and the token gating the
// board, so a half-written one would take the gate off, and a world-readable one
// would hand the key to every account on the machine.
func WriteEnvFile(path string, set map[string]string, drop []string) error {
	for key, value := range set {
		if !keyShape.MatchString(key) {
			return fmt.Errorf("%q is not a settings key", key)
		}
		if err := checkValue(key, value); err != nil {
			return err
		}
	}

	dropped := make(map[string]bool, len(drop))
	for _, key := range drop {
		dropped[key] = true
	}

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	written := make(map[string]bool, len(set))
	var out []string
	for _, line := range splitLines(string(existing)) {
		key, live := liveKey(line)
		switch {
		case !live:
			out = append(out, line)
		case dropped[key]:
			// The line goes entirely, comment and all: a superseded secret left
			// in the file is one more copy of it than anybody needs.
		case written[key]:
			// A duplicate of a key just rewritten. Keeping it would leave the
			// later line deciding what the setting is.
		default:
			if value, ok := set[key]; ok {
				out = append(out, render(key, value))
				written[key] = true
			} else {
				out = append(out, line)
			}
		}
	}

	// Appended in a stable order rather than whatever order the map ranged, so
	// rewriting the same settings twice produces the same file.
	appended := make([]string, 0, len(set))
	for key := range set {
		if !written[key] {
			appended = append(appended, key)
		}
	}
	sort.Strings(appended)
	for _, key := range appended {
		out = append(out, render(key, set[key]))
	}

	return writePrivate(path, strings.Join(out, "\n")+"\n")
}

// checkValue rejects what the format cannot carry. Both cases are refusals
// rather than escapes: a newline would append a setting of its own, and the
// reader strips outer quotes unconditionally, so a value that begins and ends
// with one cannot be read back as itself.
func checkValue(key, value string) error {
	if strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("%s cannot contain a line break", key)
	}
	trimmed := strings.TrimSpace(value)
	if len(trimmed) > 0 && (strings.HasPrefix(trimmed, `"`) || strings.HasPrefix(trimmed, "'") ||
		strings.HasSuffix(trimmed, `"`) || strings.HasSuffix(trimmed, "'")) {
		return fmt.Errorf("%s cannot begin or end with a quote", key)
	}
	return nil
}

// render writes one setting. Quotes go on only where the reader would otherwise
// lose something: it trims the whole line before reading the value, so leading
// and trailing spaces are the case that needs them.
func render(key, value string) string {
	if value != strings.TrimSpace(value) {
		return key + `="` + value + `"`
	}
	return key + "=" + value
}

// liveKey reports the key a line sets, and whether it sets one at all. A comment
// is not a setting even when it looks exactly like one — that is what a
// commented-out default is.
func liveKey(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	key, _, ok := strings.Cut(trimmed, "=")
	if !ok {
		return "", false
	}
	key = strings.TrimSpace(key)
	if !keyShape.MatchString(key) {
		return "", false
	}
	return key, true
}

// splitLines drops the trailing empty element a file ending in a newline
// produces, so rewriting a file does not grow a blank line each time.
func splitLines(data string) []string {
	if data == "" {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// writePrivate replaces the file in one step. The temporary lands in the same
// directory so the rename stays within a filesystem and is therefore atomic.
func writePrivate(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".env-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	// Fsync before the rename: a rename that lands ahead of the data leaves a
	// file that exists and says nothing, which is a server that starts with no
	// key and no token rather than one that fails to start.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
