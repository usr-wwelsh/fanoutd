package config

import (
	"os"
	"path/filepath"
)

// Source says where a setting's value came from. It matters because the two
// live sources behave differently under editing: the file is what a settings
// page writes, and an exported variable silently outrules whatever it writes,
// both now and at the next start.
type Source string

const (
	SourceEnv   Source = "env"
	SourceFile  Source = "file"
	SourceUnset Source = ""
)

// Setting is one setting as the process actually sees it.
type Setting struct {
	Value  string
	Source Source
	// Key is the name the value was actually read from, which is not always the
	// one asked for: an install predating the provider setting still names its
	// key OPENROUTER_API_KEY, and the server runs on it.
	Key string
}

// Effective reports the current value and origin of each key, using the same
// precedence Load does — including the superseded names, so a setting that is
// genuinely in force never reads as missing.
func Effective(keys []string) map[string]Setting {
	_, fileVals := loadEnvFile()

	look := func(name string) (Setting, bool) {
		if v := os.Getenv(name); v != "" {
			return Setting{Value: v, Source: SourceEnv, Key: name}, true
		}
		// A key present in the file but empty still came from the file: an
		// empty FANOUT_TOKEN is a decision to leave the API open, not an
		// absence of one.
		if v, ok := fileVals[name]; ok {
			return Setting{Value: v, Source: SourceFile, Key: name}, true
		}
		return Setting{}, false
	}

	out := make(map[string]Setting, len(keys))
	for _, key := range keys {
		found, ok := look(key)
		// An empty current name does not settle it: the older name may still
		// hold the value the process is actually using.
		for _, legacy := range LegacyNames(key) {
			if ok && found.Value != "" {
				break
			}
			if alt, altOK := look(legacy); altOK && alt.Value != "" {
				found, ok = alt, true
			}
		}
		out[key] = found
	}
	return out
}

// LegacyNames returns the superseded names for a setting, oldest meaning first.
// They are the OPENROUTER_* originals, from when there was only one provider.
func LegacyNames(key string) []string {
	return legacyNames[key]
}

var legacyNames = map[string][]string{
	"FANOUT_API_KEY":  {"OPENROUTER_API_KEY"},
	"FANOUT_MODEL":    {"OPENROUTER_MODEL"},
	"FANOUT_BASE_URL": {"OPENROUTER_BASE_URL"},
}

// SettingsFile is where an edited setting is written: whichever file the server
// already loaded, or the XDG config path when there is none. It never picks the
// working directory for a file that does not exist — a server started from
// somewhere incidental should not scatter a .env there.
func SettingsFile() string {
	if path := envFilePath(); path != "" {
		return path
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(abs(xdg), "fanoutd", "env")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "fanoutd", "env")
	}
	return abs(".env")
}
