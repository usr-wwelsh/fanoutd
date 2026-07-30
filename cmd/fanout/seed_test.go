package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write makes a file under dir, creating parents, and returns its path.
func write(t *testing.T, dir, rel, content string) string {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// A file arrives under its own name, and a directory under its own name as a
// prefix — what you would get by copying the argument into the workspace.
func TestCollectSeedNamesFilesAsCopied(t *testing.T) {
	dir := t.TempDir()
	spec := write(t, dir, "spec.md", "brief")
	write(t, dir, "docs/api.md", "api")
	write(t, dir, "docs/deep/notes.txt", "notes")

	files, err := collectSeed([]string{spec, filepath.Join(dir, "docs")})
	if err != nil {
		t.Fatalf("collectSeed: %v", err)
	}

	got := map[string]string{}
	for _, f := range files {
		got[f.Path] = f.Content
	}
	want := map[string]string{
		"spec.md":             "brief",
		"docs/api.md":         "api",
		"docs/deep/notes.txt": "notes",
	}
	if len(got) != len(want) {
		t.Fatalf("collected %v, want %v", got, want)
	}
	for path, content := range want {
		if got[path] != content {
			t.Errorf("%s = %q, want %q", path, got[path], content)
		}
	}
}

// A trailing separator is what a shell's tab completion produces, and it must
// not change the name the directory lands under.
func TestCollectSeedIgnoresATrailingSlash(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "docs/api.md", "api")

	files, err := collectSeed([]string{filepath.Join(dir, "docs") + string(os.PathSeparator)})
	if err != nil {
		t.Fatalf("collectSeed: %v", err)
	}
	if len(files) != 1 || files[0].Path != "docs/api.md" {
		t.Fatalf("collected %v, want docs/api.md", files)
	}
}

// A seed comes from a working directory. .git and .env are the two things there
// that must never reach an agent, so dotted names are skipped at every level.
func TestCollectSeedSkipsDottedNames(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "src/main.go", "package main")
	write(t, dir, "src/.env", "SECRET=1")
	write(t, dir, "src/.git/config", "[core]")
	write(t, dir, "src/pkg/.hidden", "x")

	files, err := collectSeed([]string{filepath.Join(dir, "src")})
	if err != nil {
		t.Fatalf("collectSeed: %v", err)
	}
	if len(files) != 1 || files[0].Path != "src/main.go" {
		t.Fatalf("collected %v, want only src/main.go", files)
	}
}

func TestCollectSeedRefusesBinaryAndMissingPaths(t *testing.T) {
	dir := t.TempDir()
	binary := write(t, dir, "logo.png", "\x89PNG\x00\x00")

	if _, err := collectSeed([]string{binary}); err == nil {
		t.Error("a binary file was accepted")
	}
	if _, err := collectSeed([]string{filepath.Join(dir, "nope.md")}); err == nil {
		t.Error("a missing path was accepted")
	}
	if _, err := collectSeed([]string{filepath.Join(dir, "empty-dir")}); err == nil {
		t.Error("a missing directory was accepted")
	}
}

// Two arguments with the same base name would land on one path, silently
// dropping one of them.
func TestCollectSeedRejectsACollision(t *testing.T) {
	dir := t.TempDir()
	a := write(t, dir, "a/spec.md", "one")
	b := write(t, dir, "b/spec.md", "two")

	if _, err := collectSeed([]string{a, b}); err == nil {
		t.Error("two files named spec.md were accepted")
	}
}

func TestCollectSeedRejectsAnOversizeFile(t *testing.T) {
	dir := t.TempDir()
	big := write(t, dir, "big.txt", strings.Repeat("x", maxSeedFileBytes+1))

	if _, err := collectSeed([]string{big}); err == nil {
		t.Error("an oversize file was accepted")
	}
}

func TestCollectSeedIsEmptyWithoutTheFlag(t *testing.T) {
	files, err := collectSeed(nil)
	if err != nil {
		t.Fatalf("collectSeed(nil): %v", err)
	}
	if len(files) != 0 {
		t.Errorf("collected %v, want nothing", files)
	}
}
