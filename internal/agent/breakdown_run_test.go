package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fanoutd/internal/llm"
	"fanoutd/internal/models"
	"fanoutd/internal/store"
)

// fakeBreakdown answers the breakdown call from a queue of canned plans and
// hands everything else to the subtask fake, so one server covers a whole
// breakdown-then-run. A nil client would panic the moment a schedule reached the
// model, which is exactly what these tests are for.
type fakeBreakdown struct {
	subtasks *fakeModel

	mu      sync.Mutex
	replies []string
	prompts []string
}

func (f *fakeBreakdown) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if !isBreakdownRequest(body) {
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.subtasks.ServeHTTP(w, r)
		return
	}

	f.mu.Lock()
	f.prompts = append(f.prompts, string(body))
	reply := "not a plan"
	if len(f.replies) > 0 {
		reply, f.replies = f.replies[0], f.replies[1:]
	}
	f.mu.Unlock()

	frame, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": reply}}},
	})
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
}

func (f *fakeBreakdown) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.prompts...)
}

// isBreakdownRequest tells the planning call from a subtask's step by the system
// prompt it carries.
func isBreakdownRequest(body []byte) bool {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return false
	}
	return req.Messages[0].Role == "system" && strings.Contains(req.Messages[0].Content, `"subtasks"`)
}

func breakdownLoop(t *testing.T, replies ...string) (*Loop, *store.Store, *fakeBreakdown) {
	t.Helper()
	f := &fakeBreakdown{subtasks: &fakeModel{}, replies: replies}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	s, err := store.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	client := llm.NewClient(llm.Preset{Name: "test"}, "test-key", "test-model", srv.URL)
	return NewLoop(s, client, filepath.Join(dir, "output")), s, f
}

const goodPlan = `{"contract": "board.js exports mount(el) and reads schema.json for its cells",
 "subtasks": [
  {"title": "schema", "goal": "write the schema", "writes": ["schema.json"], "reads": [], "criteria": ["schema.json parses as JSON"]},
  {"title": "impl", "goal": "write the board", "writes": ["board.js"], "reads": ["schema.json"], "criteria": ["mount(el) renders one cell per schema entry"]},
  {"title": "page", "goal": "write the page", "writes": ["index.html"], "reads": ["board.js"], "integration": true, "criteria": ["index.html opens from file:// with no console errors"]}
]}`

// Two subtasks writing board.js: the one failure the retry exists for.
const conflictingPlan = `{"subtasks": [
  {"title": "impl", "goal": "write the board", "writes": ["board.js"], "reads": []},
  {"title": "tests", "goal": "test the board", "writes": ["board.js", "test.js"], "reads": []}
]}`

func TestBreakdownBuildsAGroup(t *testing.T) {
	l, s, _ := breakdownLoop(t, goodPlan)

	result, err := l.Breakdown(context.Background(), BreakdownRequest{Idea: "build a board"})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if result.Fallback != "" {
		t.Fatalf("fell back on a good plan: %s", result.Fallback)
	}
	if len(result.Tasks) != 3 {
		t.Fatalf("got %d tasks, want 3", len(result.Tasks))
	}

	// One group, one workspace: claims are scoped to the workspace, so subtasks
	// spread across two would silently lose every dependency between them.
	workspace := result.Tasks[0].WorkspaceID
	for _, task := range result.Tasks {
		if task.GroupID != result.GroupID {
			t.Errorf("task %s group = %q, want %q", task.Title, task.GroupID, result.GroupID)
		}
		if task.WorkspaceID != workspace {
			t.Errorf("task %s workspace = %q, want %q", task.Title, task.WorkspaceID, workspace)
		}
	}

	writers, err := s.Writers(workspace)
	if err != nil {
		t.Fatalf("Writers: %v", err)
	}
	byTitle := map[string]string{}
	for _, task := range result.Tasks {
		byTitle[task.Title] = task.ID
	}
	for path, title := range map[string]string{
		"schema.json": "schema", "board.js": "impl", "index.html": "page",
	} {
		if writers[path] != byTitle[title] {
			t.Errorf("%s is owned by %q, want %q", path, writers[path], title)
		}
	}

	// The reads were declared, so the graph is a chain rather than three
	// subtasks that all look independent.
	want := [][]string{{byTitle["schema"]}, {byTitle["impl"]}, {byTitle["page"]}}
	if len(result.Plan.Waves) != 3 {
		t.Fatalf("waves = %v, want three serial waves", result.Plan.Waves)
	}
	for i := range want {
		if len(result.Plan.Waves[i]) != 1 || result.Plan.Waves[i][0] != want[i][0] {
			t.Fatalf("waves = %v, want %v", result.Plan.Waves, want)
		}
	}
}

func TestBreakdownRetriesOnceOnAConflict(t *testing.T) {
	l, _, f := breakdownLoop(t, conflictingPlan, goodPlan)

	result, err := l.Breakdown(context.Background(), BreakdownRequest{Idea: "build a board"})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if result.Fallback != "" {
		t.Fatalf("fell back despite a good second plan: %s", result.Fallback)
	}
	if len(result.Tasks) != 3 {
		t.Errorf("got %d tasks, want the corrected plan's 3", len(result.Tasks))
	}

	sent := f.sent()
	if len(sent) != 2 {
		t.Fatalf("made %d planning calls, want exactly 2", len(sent))
	}
	// The retry has to name the conflict, or it is just the same question again.
	if !strings.Contains(sent[1], "board.js") {
		t.Errorf("the re-plan prompt does not name the contested path:\n%s", sent[1])
	}
	// The rejected plan is left behind as tasks nobody asked for if the first
	// attempt created anything, which is why nothing is created until a plan
	// passes validation.
	if strings.Count(sent[1], `"role":"user"`) < 2 {
		t.Errorf("the retry should continue the conversation, not restart it:\n%s", sent[1])
	}
}

func TestBreakdownFallsBackAfterTwoBadPlans(t *testing.T) {
	l, s, f := breakdownLoop(t, conflictingPlan, conflictingPlan)

	idea := "build a board with tests"
	result, err := l.Breakdown(context.Background(), BreakdownRequest{Idea: idea})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if result.Fallback == "" {
		t.Fatal("a twice-conflicting plan produced a group, want the single-task floor")
	}
	if result.GroupID != "" {
		t.Errorf("the fallback carries a group id %q", result.GroupID)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(result.Tasks))
	}
	// The floor has to carry the original idea, or falling back loses the work.
	if result.Tasks[0].Goal != idea {
		t.Errorf("fallback goal = %q, want the original idea", result.Tasks[0].Goal)
	}
	if result.Tasks[0].GroupID != "" {
		t.Errorf("the fallback task is in group %q; nothing arbitrates a lone task's writes", result.Tasks[0].GroupID)
	}
	if n := len(f.sent()); n != 2 {
		t.Errorf("made %d planning calls, want 2", n)
	}

	// Nothing half-built: the rejected plans left no rows and no claims.
	tasks, err := s.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("board holds %d tasks after a fallback, want only the single task", len(tasks))
	}
}

func TestBreakdownFallsBackWhenTheModelIsUnreachable(t *testing.T) {
	l, _, _ := breakdownLoop(t)
	// A cancelled context stands in for any transport failure; the point is that
	// it produces a task rather than nothing.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := l.Breakdown(ctx, BreakdownRequest{Idea: "build a board", Title: "Board"})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if result.Fallback == "" || len(result.Tasks) != 1 {
		t.Fatalf("got %+v, want the single-task floor", result)
	}
	if result.Tasks[0].Title != "Board" {
		t.Errorf("title = %q, want the caller's", result.Tasks[0].Title)
	}
}

func TestBreakdownRejectsAnEmptyIdea(t *testing.T) {
	l, _, _ := breakdownLoop(t)
	if _, err := l.Breakdown(context.Background(), BreakdownRequest{Idea: "  "}); err == nil {
		t.Error("an empty idea was accepted")
	}
}

func TestBreakdownStartsAndRunsTheSchedule(t *testing.T) {
	l, s, _ := breakdownLoop(t, goodPlan)

	result, err := l.Breakdown(context.Background(), BreakdownRequest{Idea: "build a board", Start: true})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if !result.Started {
		t.Fatal("Start was requested but the schedule did not launch")
	}
	t.Cleanup(func() { l.StopGroup(result.GroupID) })

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && l.IsGroupRunning(result.GroupID) {
		time.Sleep(20 * time.Millisecond)
	}
	if l.IsGroupRunning(result.GroupID) {
		t.Fatal("the group never finished")
	}

	tasks, err := s.TasksInGroup(result.GroupID)
	if err != nil {
		t.Fatalf("TasksInGroup: %v", err)
	}
	for _, task := range tasks {
		if task.Status != models.StatusDone {
			t.Errorf("%s status = %q (%s), want done", task.Title, task.Status, task.Error)
		}
	}
}

// Every subtask's prompt has to carry the idea it came from, worded so the agent
// does not try to build the whole thing and lose every file it does not own.
func TestBreakdownGivesSubtasksTheirContext(t *testing.T) {
	l, _, _ := breakdownLoop(t, goodPlan)
	result, err := l.Breakdown(context.Background(), BreakdownRequest{Idea: "build a kanban board"})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	for _, task := range result.Tasks {
		if !strings.Contains(task.Description, "build a kanban board") {
			t.Errorf("%s has no record of the idea it came from: %q", task.Title, task.Description)
		}
		if !strings.Contains(task.Description, "Sibling subtasks own the rest") {
			t.Errorf("%s is not told to stay in its lane: %q", task.Title, task.Description)
		}
	}
}

// The idea is not stored anywhere but the subtask descriptions, so the board can
// only label a group by reading it back out. That makes the round trip a
// contract rather than a convenience.
func TestGroupIdeaIsRecoverableFromASubtask(t *testing.T) {
	l, _, _ := breakdownLoop(t, goodPlan)
	result, err := l.Breakdown(context.Background(), BreakdownRequest{Idea: "build a kanban board"})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	for _, task := range result.Tasks {
		if got := GroupIdea(task.Description); got != "build a kanban board" {
			t.Errorf("%s: GroupIdea = %q, want the original idea", task.Title, got)
		}
	}

	// Anything this package did not write has no idea to recover, and must say
	// so rather than return the description as though it were one.
	for _, desc := range []string{"", "a description someone typed", ideaPrefix} {
		if got := GroupIdea(desc); got != "" {
			t.Errorf("GroupIdea(%q) = %q, want empty", desc, got)
		}
	}
}

// The events are what a client watches while a split runs, so their order is
// part of the contract: each stage is announced before its work happens, and
// the planner's output arrives between them as progress.
func TestBreakdownReportsItsProgress(t *testing.T) {
	l, _, _ := breakdownLoop(t, goodPlan)

	var events []models.BreakdownEvent
	req := BreakdownRequest{Idea: "build a board", Events: func(e models.BreakdownEvent) {
		events = append(events, e)
	}}
	if _, err := l.Breakdown(context.Background(), req); err != nil {
		t.Fatalf("Breakdown: %v", err)
	}

	var phases []string
	progressed := false
	for _, e := range events {
		switch e.Kind {
		case models.BreakdownKindPhase:
			phases = append(phases, e.Phase)
		case models.BreakdownKindProgress:
			progressed = true
			if e.Chars == 0 || e.Tail == "" {
				t.Errorf("progress event carries nothing: %+v", e)
			}
			if !strings.Contains(e.Tail, "subtasks") {
				t.Errorf("progress tail lost the plan itself: %q", e.Tail)
			}
		}
	}
	want := []string{models.PhasePlanning, models.PhaseBuilding}
	if len(phases) != len(want) {
		t.Fatalf("phases = %v, want exactly %v", phases, want)
	}
	for i := range want {
		if phases[i] != want[i] {
			t.Fatalf("phases = %v, want %v", phases, want)
		}
	}
	if !progressed {
		t.Error("a streamed plan produced no progress events")
	}
}

// A rejected plan must not look like silence: the replanning stage names what
// was wrong with it, which is the same fault the model itself is shown.
func TestBreakdownNamesTheFaultWhenItReplans(t *testing.T) {
	l, _, _ := breakdownLoop(t, conflictingPlan, goodPlan)

	var replan *models.BreakdownEvent
	req := BreakdownRequest{Idea: "build a board", Events: func(e models.BreakdownEvent) {
		if e.Kind == models.BreakdownKindPhase && e.Phase == models.PhaseReplanning {
			replan = &e
		}
	}}
	if _, err := l.Breakdown(context.Background(), req); err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if replan == nil {
		t.Fatal("the second attempt was never announced as a replanning")
	}
	if !strings.Contains(replan.Note, "board.js") {
		t.Errorf("the replanning note does not name the contested path: %q", replan.Note)
	}
}

// The floor has its own announcement, so a client can say why the idea ended
// up as one task while it happens rather than after.
func TestBreakdownAnnouncesTheFallback(t *testing.T) {
	l, _, _ := breakdownLoop(t)

	var log eventLog
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := l.Breakdown(ctx, BreakdownRequest{Idea: "build a board"}.WithEvents(&log))
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if result.Fallback == "" {
		t.Fatal("want the single-task floor")
	}
	if log.phase(models.PhaseFallback) == nil {
		t.Error("the fallback was never announced")
	}
}

// eventLog collects what Breakdown emits and answers the small questions a
// test wants to ask of it.
type eventLog struct {
	events []models.BreakdownEvent
}

func (l *eventLog) record(e models.BreakdownEvent) {
	l.events = append(l.events, e)
}

func (l *eventLog) phase(phase string) *models.BreakdownEvent {
	for i := range l.events {
		e := &l.events[i]
		if e.Kind == models.BreakdownKindPhase && e.Phase == phase {
			return e
		}
	}
	return nil
}

// WithEvents returns the receiver carrying an observer that appends to log.
// It exists so a test can ask for one request's worth of events inline.
func (req BreakdownRequest) WithEvents(log *eventLog) BreakdownRequest {
	req.Events = log.record
	return req
}

// buildGroup is reached with a validated plan, so the only way its own checks
// fire is a race. Driving it directly is how the unwind gets covered - a group
// that cannot be scheduled must leave nothing behind for the fallback to trip
// over.
func TestBuildGroupUnwindsAnUnschedulablePlan(t *testing.T) {
	l, s, _ := breakdownLoop(t)

	_, err := l.buildGroup(BreakdownRequest{Idea: "x"}, planOf(
		Subtask{Title: "a", Goal: "g", Writes: []string{"a.md"}, Reads: []string{"b.md"}},
		Subtask{Title: "b", Goal: "g", Writes: []string{"b.md"}, Reads: []string{"a.md"}},
	))
	if err == nil {
		t.Fatal("a cyclic plan was built, want it refused")
	}

	tasks, err := s.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	// The rows go, and DeleteTask takes their claims with them - otherwise the
	// paths stay owned by tasks that no longer exist.
	if len(tasks) != 0 {
		t.Errorf("%d tasks survived a rejected plan: %+v", len(tasks), tasks)
	}
}
