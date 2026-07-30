package main

import (
	"bytes"
	"flag"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"fanoutd/internal/models"
)

// Seeding reads local paths here, in the client, and sends the contents. The
// server may be on another machine — that is the whole point of the config file
// — so a path is only meaningful on the side that typed it.

// The server enforces its own limits; these exist so a mistyped --seed fails on
// this side with the offending path named, rather than as a 400.
const (
	maxSeedFileBytes  = 256 * 1024
	maxSeedTotalBytes = 2 * 1024 * 1024
	maxSeedFiles      = 200
)

// seedList collects a repeated --seed flag.
type seedList []string

func (s *seedList) String() string { return strings.Join(*s, ", ") }

func (s *seedList) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("a path is required")
	}
	*s = append(*s, v)
	return nil
}

// seedFlag registers --seed on a command. The returned pointer is read after
// parsing, like every other flag here.
func seedFlag(fs *flag.FlagSet) *seedList {
	var list seedList
	fs.Var(&list, "seed", "copy a file or directory into the workspace before the run; repeatable")
	return &list
}

// collectSeed turns the given paths into the files to send. A file keeps its own
// name; a directory keeps its name as a prefix, so `--seed docs` arrives as
// docs/... and `--seed spec.md` as spec.md. Both are what you get if you had
// copied the argument into the workspace directory.
func collectSeed(paths []string) ([]models.SeedFile, error) {
	var out []models.SeedFile
	total := 0
	seen := map[string]bool{}

	for _, arg := range paths {
		clean := filepath.Clean(strings.TrimRight(arg, string(os.PathSeparator)))
		info, err := os.Stat(clean)
		if err != nil {
			return nil, fmt.Errorf("--seed %s: %w", arg, err)
		}

		abs, err := filepath.Abs(clean)
		if err != nil {
			return nil, err
		}
		base := filepath.Base(abs)
		if base == "" || base == string(os.PathSeparator) {
			return nil, fmt.Errorf("--seed %s: cannot name a file in the workspace from that path", arg)
		}

		if !info.IsDir() {
			file, err := readSeedFile(clean, base)
			if err != nil {
				return nil, err
			}
			if err := addSeed(&out, &total, seen, *file); err != nil {
				return nil, err
			}
			continue
		}

		walked, err := walkSeedDir(abs, base)
		if err != nil {
			return nil, err
		}
		if len(walked) == 0 {
			return nil, fmt.Errorf("--seed %s: no readable text files in it", arg)
		}
		for _, f := range walked {
			if err := addSeed(&out, &total, seen, f); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// walkSeedDir gathers a directory's text files, skipping dotted names at every
// level. A seed comes from a working directory, and .git and .env are the two
// things there that must not be handed to an agent.
func walkSeedDir(root, prefix string) ([]models.SeedFile, error) {
	var out []models.SeedFile
	err := filepath.WalkDir(root, func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if path != root && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Symlinks are not followed: a link out of the tree would seed a file the
		// argument does not name.
		if !d.Type().IsRegular() {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file, err := readSeedFile(path, filepath.ToSlash(filepath.Join(prefix, rel)))
		if err != nil {
			return err
		}
		out = append(out, *file)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// readSeedFile reads one file under dest. Binary content is refused rather than
// mangled: it travels as a JSON string and the agent's tools only handle text, so
// a seeded image would arrive corrupted and be unreadable anyway.
func readSeedFile(path, dest string) (*models.SeedFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--seed: %w", err)
	}
	if len(data) > maxSeedFileBytes {
		return nil, fmt.Errorf("--seed: %s is %d bytes, over the %d limit", path, len(data), maxSeedFileBytes)
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return nil, fmt.Errorf("--seed: %s is not text; only text files can be seeded", path)
	}
	return &models.SeedFile{Path: filepath.ToSlash(dest), Content: string(data)}, nil
}

func addSeed(out *[]models.SeedFile, total *int, seen map[string]bool, f models.SeedFile) error {
	if seen[f.Path] {
		return fmt.Errorf("--seed: %s would be seeded twice", f.Path)
	}
	if len(*out) >= maxSeedFiles {
		return fmt.Errorf("--seed: more than %d files; seed a narrower directory", maxSeedFiles)
	}
	*total += len(f.Content)
	if *total > maxSeedTotalBytes {
		return fmt.Errorf("--seed: over the %d byte total; seed a narrower directory", maxSeedTotalBytes)
	}
	seen[f.Path] = true
	*out = append(*out, f)
	return nil
}

// describeSeed is the one line printed after a seeded command, so it is clear
// what the agent started with.
func describeSeed(files []models.SeedFile) string {
	if len(files) == 0 {
		return ""
	}
	total := 0
	for _, f := range files {
		total += len(f.Content)
	}
	return fmt.Sprintf("seeded %s (%d bytes)", plural(len(files), "file"), total)
}
