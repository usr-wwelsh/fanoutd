package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"fanoutd/internal/models"
)

// Seeded files are written unclaimed. Nothing owns them, so any subtask may read
// them and one may overwrite a path by declaring it in "writes" — which is how a
// breakdown revises material it was handed rather than only adding to it.

// The bounds are enforced here rather than in the CLI alone, because the CLI is
// not the only thing that can POST a seed. They are sized for text an agent will
// actually read: a large file is a file the model cannot fit in context anyway.
const (
	MaxSeedFileBytes  = 256 * 1024
	MaxSeedTotalBytes = 2 * 1024 * 1024
	MaxSeedFiles      = 200
)

// Seed writes files into a workspace before any task runs. The workspace must be
// the unarbitrated view — passing an Owned one would claim every seeded path for
// a single task.
func Seed(ws *Workspace, files []models.SeedFile) error {
	if len(files) == 0 {
		return nil
	}
	if err := ValidateSeed(files); err != nil {
		return err
	}
	if err := os.MkdirAll(ws.Root(), 0o755); err != nil {
		return err
	}

	for _, f := range files {
		full, err := ws.resolve(f.Path)
		if err != nil {
			return fmt.Errorf("seed %s: %w", f.Path, err)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(f.Content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// SeedTask installs a seed into the workspace backing a task, which is what a
// freshly created single task needs before it is started.
func (l *Loop) SeedTask(taskID string, files []models.SeedFile) error {
	if len(files) == 0 {
		return nil
	}
	ws, err := l.Workspace(taskID)
	if err != nil {
		return err
	}
	return Seed(ws, files)
}

// seedWorkspace installs a seed into a workspace named directly, for the
// breakdown paths that have the shared ID but no single task to resolve through.
func (l *Loop) seedWorkspace(workspaceID string, files []models.SeedFile) error {
	if len(files) == 0 {
		return nil
	}
	ws, err := NewWorkspace(l.outputDir, workspaceID)
	if err != nil {
		return err
	}
	return Seed(ws, files)
}

// ValidateSeed rejects a seed set before any of it is written, so a request that
// is too large fails with nothing half-installed.
func ValidateSeed(files []models.SeedFile) error {
	if len(files) > MaxSeedFiles {
		return fmt.Errorf("seed holds %d files, more than the %d limit", len(files), MaxSeedFiles)
	}
	total := 0
	seen := map[string]bool{}
	for _, f := range files {
		key, ok := normalizeClaimPath(f.Path)
		if !ok {
			return fmt.Errorf("seed path %q cannot be used", f.Path)
		}
		if seen[key] {
			return fmt.Errorf("seed names %s twice", key)
		}
		seen[key] = true

		if len(f.Content) > MaxSeedFileBytes {
			return fmt.Errorf("seed file %s is %d bytes, over the %d limit", key, len(f.Content), MaxSeedFileBytes)
		}
		total += len(f.Content)
		if total > MaxSeedTotalBytes {
			return fmt.Errorf("seed is over the %d byte total", MaxSeedTotalBytes)
		}
	}
	return nil
}

// seedBrief tells the planner what is already on disk. It has to amend the
// system prompt's rule that "reads" holds only a sibling's output: a seeded file
// is readable by everyone and creates no ordering, so listing it there would
// invent a dependency on a subtask that does not exist.
func seedBrief(files []models.SeedFile) string {
	manifest := seedManifest(files)
	if manifest == "" {
		return ""
	}
	return "\n\nThese files are already in the shared directory before any subtask starts:\n" +
		manifest + "\n" +
		`Any subtask may read them with read_file, and should be told in its goal to
do so. Leave them out of "reads" — that field is only for a file a sibling in
this plan writes, and these have no writer to wait for. Put one in "writes" only
if that subtask's job is to replace it.`
}

// seedManifest lists the seeded paths with their sizes, for the breakdown
// planner. Sorted so the same seed always produces the same prompt.
func seedManifest(files []models.SeedFile) string {
	lines := make([]string, 0, len(files))
	for _, f := range files {
		key, ok := normalizeClaimPath(f.Path)
		if !ok {
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s (%d bytes)", key, len(f.Content)))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
