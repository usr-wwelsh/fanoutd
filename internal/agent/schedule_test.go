package agent

import (
	"reflect"
	"strings"
	"testing"

	"fanoutd/internal/store"
)

func TestDeriveDepsFromFilePartition(t *testing.T) {
	ids := []string{"schema", "impl", "tests"}
	writers := map[string]string{
		"schema.json":   "schema",
		"board.js":      "impl",
		"board_test.js": "tests",
	}
	reads := map[string][]string{
		"impl":  {"schema.json"},
		"tests": {"schema.json", "board.js"},
	}

	deps := deriveDeps(ids, reads, writers)

	if got := deps["schema"]; len(got) != 0 {
		t.Errorf("schema deps = %v, want none", got)
	}
	if got := deps["impl"]; !reflect.DeepEqual(got, []string{"schema"}) {
		t.Errorf("impl deps = %v, want [schema]", got)
	}
	// The case the roadmap called out: tests and the thing they test are not
	// parallel, and nothing had to say so — the file lists did.
	if got := deps["tests"]; !reflect.DeepEqual(got, []string{"impl", "schema"}) {
		t.Errorf("tests deps = %v, want [impl schema]", got)
	}
}

func TestDeriveDepsIgnoresNonEdges(t *testing.T) {
	ids := []string{"a", "b"}
	writers := map[string]string{
		"a.md":       "a",
		"outside.md": "stranger", // written by a task outside the group
	}
	reads := map[string][]string{
		"a": {"a.md"}, // reading what you write is not an edge
		"b": {"missing.md", "outside.md", "a.md", "a.md"},
	}

	deps := deriveDeps(ids, reads, writers)

	if got := deps["a"]; len(got) != 0 {
		t.Errorf("self-read produced deps %v, want none", got)
	}
	// A path nobody in the group writes cannot be waited on, and a repeated
	// read must not duplicate the edge.
	if got := deps["b"]; !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("b deps = %v, want [a]", got)
	}
}

func TestTopoWaves(t *testing.T) {
	ids := []string{"schema", "impl", "docs", "tests"}
	deps := map[string][]string{
		"impl":  {"schema"},
		"tests": {"impl"},
		"docs":  {"schema"},
	}

	waves, err := topoWaves(ids, deps)
	if err != nil {
		t.Fatalf("topoWaves: %v", err)
	}
	want := [][]string{{"schema"}, {"impl", "docs"}, {"tests"}}
	if !reflect.DeepEqual(waves, want) {
		t.Errorf("waves = %v, want %v", waves, want)
	}
}

func TestTopoWavesDetectsCycle(t *testing.T) {
	ids := []string{"aaaaaaaaaa", "bbbbbbbbbb", "cccccccccc"}
	deps := map[string][]string{
		"aaaaaaaaaa": {"bbbbbbbbbb"},
		"bbbbbbbbbb": {"aaaaaaaaaa"},
	}

	_, err := topoWaves(ids, deps)
	if err == nil {
		t.Fatal("cycle went undetected")
	}
	// The report has to name the tasks, or a user cannot fix the breakdown.
	if !strings.Contains(err.Error(), "aaaaaaa") || !strings.Contains(err.Error(), "bbbbbbb") {
		t.Errorf("cycle error %q does not name the tasks involved", err)
	}
	// The task outside the cycle is not implicated.
	if strings.Contains(err.Error(), "ccccccc") {
		t.Errorf("cycle error %q implicates a task that is not in the cycle", err)
	}
}

func TestTopoWavesEmpty(t *testing.T) {
	waves, err := topoWaves(nil, nil)
	if err != nil {
		t.Fatalf("topoWaves: %v", err)
	}
	if len(waves) != 0 {
		t.Errorf("waves = %v, want none", waves)
	}
}

// makeGroup creates a breakdown sharing one workspace and declares its file
// partition, which is all PlanGroup reads.
func makeGroup(t *testing.T, s *store.Store, specs []struct {
	title  string
	writes []string
	reads  []string
}) (string, string, map[string]string) {
	t.Helper()
	groupID, workspace := "grp1", "ws1"
	byTitle := map[string]string{}
	for _, spec := range specs {
		task, err := s.CreateTaskFrom(store.NewTask{
			Title:       spec.title,
			Goal:        spec.title,
			GroupID:     groupID,
			WorkspaceID: workspace,
		})
		if err != nil {
			t.Fatalf("CreateTaskFrom: %v", err)
		}
		byTitle[spec.title] = task.ID
		conflicts, err := s.DeclareWrites(workspace, task.ID, spec.writes)
		if err != nil {
			t.Fatalf("DeclareWrites: %v", err)
		}
		if len(conflicts) > 0 {
			t.Fatalf("unexpected conflicts in fixture: %+v", conflicts)
		}
		if err := s.DeclareReads(workspace, task.ID, spec.reads); err != nil {
			t.Fatalf("DeclareReads: %v", err)
		}
	}
	return groupID, workspace, byTitle
}

func TestPlanGroupResolvesSchedule(t *testing.T) {
	l, s := testLoop(t)
	groupID, _, ids := makeGroup(t, s, []struct {
		title  string
		writes []string
		reads  []string
	}{
		{"schema", []string{"schema.json"}, nil},
		{"impl", []string{"board.js"}, []string{"schema.json"}},
		{"tests", []string{"board_test.js"}, []string{"board.js"}},
	})

	plan, err := l.PlanGroup(groupID)
	if err != nil {
		t.Fatalf("PlanGroup: %v", err)
	}
	if len(plan.Waves) != 3 {
		t.Fatalf("waves = %v, want three serial waves", plan.Waves)
	}
	if plan.Waves[0][0] != ids["schema"] {
		t.Errorf("first wave = %v, want the schema task alone", plan.Waves[0])
	}
	if got := plan.Deps[ids["tests"]]; !reflect.DeepEqual(got, []string{ids["impl"]}) {
		t.Errorf("tests deps = %v, want [impl]", got)
	}
}

func TestPlanGroupIndependentSubtasksShareAWave(t *testing.T) {
	l, s := testLoop(t)
	groupID, _, _ := makeGroup(t, s, []struct {
		title  string
		writes []string
		reads  []string
	}{
		{"a", []string{"a.md"}, nil},
		{"b", []string{"b.md"}, nil},
		{"c", []string{"c.md"}, nil},
	})

	plan, err := l.PlanGroup(groupID)
	if err != nil {
		t.Fatalf("PlanGroup: %v", err)
	}
	if len(plan.Waves) != 1 || len(plan.Waves[0]) != 3 {
		t.Errorf("waves = %v, want all three in one wave", plan.Waves)
	}
}

func TestPlanGroupRejectsUnknownGroup(t *testing.T) {
	l, _ := testLoop(t)
	if _, err := l.PlanGroup("nope"); err == nil {
		t.Error("planning an empty group succeeded, want an error")
	}
}

func TestPlanGroupRejectsSplitWorkspace(t *testing.T) {
	l, s := testLoop(t)
	for i, ws := range []string{"ws1", "ws2"} {
		if _, err := s.CreateTaskFrom(store.NewTask{
			Title:       string(rune('a' + i)),
			GroupID:     "grp1",
			WorkspaceID: ws,
		}); err != nil {
			t.Fatalf("CreateTaskFrom: %v", err)
		}
	}
	// Claims are scoped to a workspace, so a group spanning two would quietly
	// lose every edge between them rather than fail.
	if _, err := l.PlanGroup("grp1"); err == nil {
		t.Error("planning a split-workspace group succeeded, want an error")
	}
}

func TestSetMaxParallelIgnoresNonsense(t *testing.T) {
	l, _ := testLoop(t)
	if got := l.parallelLimit(); got != defaultMaxParallel {
		t.Fatalf("default limit = %d, want %d", got, defaultMaxParallel)
	}
	l.SetMaxParallel(0)
	l.SetMaxParallel(-4)
	if got := l.parallelLimit(); got != defaultMaxParallel {
		t.Errorf("limit = %d after nonsense values, want the default kept", got)
	}
	l.SetMaxParallel(8)
	if got := l.parallelLimit(); got != 8 {
		t.Errorf("limit = %d, want 8", got)
	}
}
