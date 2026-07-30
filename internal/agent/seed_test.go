package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fanoutd/internal/models"
)

func TestSeedWritesNestedFilesUnclaimed(t *testing.T) {
	dir := t.TempDir()
	ws, err := NewWorkspace(dir, "ws1")
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}

	seed := []models.SeedFile{
		{Path: "spec.md", Content: "the brief"},
		{Path: "docs/api/notes.txt", Content: "nested"},
	}
	if err := Seed(ws, seed); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	for _, f := range seed {
		got, err := os.ReadFile(filepath.Join(dir, "ws1", filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatalf("reading %s: %v", f.Path, err)
		}
		if string(got) != f.Content {
			t.Errorf("%s = %q, want %q", f.Path, got, f.Content)
		}
	}

	// Nothing may own a seeded path, or the first subtask told to revise one
	// would be refused its own material.
	claims := newFakeClaims()
	owned := ws.Owned("task-1", claims)
	if _, err := owned.writeFile("spec.md", "revised"); err != nil {
		t.Fatalf("a subtask could not claim a seeded path: %v", err)
	}
}

func TestSeedRejectsPathsOutsideTheWorkspace(t *testing.T) {
	dir := t.TempDir()
	ws, err := NewWorkspace(filepath.Join(dir, "output"), "ws1")
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}

	for _, path := range []string{"../escaped.md", "docs/../../escaped.md", "", "."} {
		if err := Seed(ws, []models.SeedFile{{Path: path, Content: "x"}}); err == nil {
			t.Errorf("Seed(%q) was accepted", path)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped.md")); !os.IsNotExist(err) {
		t.Error("a seed escaped the workspace root")
	}
}

func TestValidateSeedBounds(t *testing.T) {
	tests := []struct {
		name  string
		files []models.SeedFile
	}{
		{"oversize file", []models.SeedFile{{Path: "big.txt", Content: strings.Repeat("x", MaxSeedFileBytes+1)}}},
		{"duplicate path", []models.SeedFile{{Path: "a.txt", Content: "1"}, {Path: "./a.txt", Content: "2"}}},
		{"unusable path", []models.SeedFile{{Path: "..", Content: "1"}}},
	}
	for _, tc := range tests {
		if err := ValidateSeed(tc.files); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}

	// The total is what stops a wide directory, since each file may be legal.
	page := strings.Repeat("x", MaxSeedFileBytes)
	var wide []models.SeedFile
	for i := 0; i*MaxSeedFileBytes <= MaxSeedTotalBytes; i++ {
		wide = append(wide, models.SeedFile{Path: "f" + string(rune('a'+i)) + ".txt", Content: page})
	}
	if err := ValidateSeed(wide); err == nil {
		t.Error("a seed over the total was accepted")
	}
}

// Nothing is written when the set is rejected: the check runs first so a caller
// does not have to clean up after a partial install.
func TestSeedWritesNothingWhenRejected(t *testing.T) {
	dir := t.TempDir()
	ws, err := NewWorkspace(dir, "ws1")
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	err = Seed(ws, []models.SeedFile{
		{Path: "good.md", Content: "fine"},
		{Path: "big.txt", Content: strings.Repeat("x", MaxSeedFileBytes+1)},
	})
	if err == nil {
		t.Fatal("Seed accepted an oversize file")
	}
	if _, err := os.Stat(filepath.Join(dir, "ws1", "good.md")); !os.IsNotExist(err) {
		t.Error("a rejected seed left good.md behind")
	}
}

// The planner has to be told what is already on disk, or it will plan a subtask
// to produce a file that is sitting there.
func TestBreakdownShowsTheSeedToThePlanner(t *testing.T) {
	l, _, fake := breakdownLoop(t, goodPlan)

	result, err := l.Breakdown(context.Background(), BreakdownRequest{
		Idea: "build a board",
		Seed: []models.SeedFile{{Path: "spec.md", Content: "the brief"}},
	})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if result.Fallback != "" {
		t.Fatalf("fell back: %s", result.Fallback)
	}

	sent := fake.sent()
	if len(sent) == 0 {
		t.Fatal("no breakdown call was made")
	}
	if !strings.Contains(sent[0], "spec.md (9 bytes)") {
		t.Errorf("the planning prompt did not list the seed:\n%s", sent[0])
	}

	// And it is on disk before the schedule can run.
	root := filepath.Join(l.outputDir, result.Tasks[0].WorkspaceID)
	got, err := os.ReadFile(filepath.Join(root, "spec.md"))
	if err != nil {
		t.Fatalf("reading the seeded file: %v", err)
	}
	if string(got) != "the brief" {
		t.Errorf("spec.md = %q, want %q", got, "the brief")
	}
}

// A seed is material for the idea, not for the shape of the plan, so the idea
// still gets it when the split fails.
func TestSeedReachesTheFallbackTask(t *testing.T) {
	l, _, _ := breakdownLoop(t, "not a plan", "still not a plan")

	result, err := l.Breakdown(context.Background(), BreakdownRequest{
		Idea: "build a board",
		Seed: []models.SeedFile{{Path: "spec.md", Content: "the brief"}},
	})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if result.Fallback == "" {
		t.Fatal("expected a fallback")
	}

	root := filepath.Join(l.outputDir, result.Tasks[0].WorkspaceID)
	if _, err := os.ReadFile(filepath.Join(root, "spec.md")); err != nil {
		t.Fatalf("the fallback task was not seeded: %v", err)
	}
}

func TestSeedBriefIsEmptyWithoutASeed(t *testing.T) {
	if got := seedBrief(nil); got != "" {
		t.Errorf("seedBrief(nil) = %q, want empty", got)
	}
}
