package agent

import (
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

	"fanoutd/internal/models"
	"fanoutd/internal/openrouter"
	"fanoutd/internal/store"
)

// fakeModel is an OpenRouter-shaped server that answers every step with a
// finish call, so a scheduled run reaches "done" without a real provider. It
// records which goal each request was for and how many were in flight at once,
// which is what the schedule is actually being judged on.
type fakeModel struct {
	mu       sync.Mutex
	order    []string
	inFlight int
	peak     int
	delay    time.Duration
	// failGoals names subtasks the provider should reject, for testing what a
	// broken upstream does to everything downstream of it.
	failGoals map[string]bool
}

func (f *fakeModel) enter(goal string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, goal)
	f.inFlight++
	if f.inFlight > f.peak {
		f.peak = f.inFlight
	}
}

func (f *fakeModel) leave() {
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
}

func (f *fakeModel) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func (f *fakeModel) peakParallel() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	goal := goalFromRequest(body)

	f.enter(goal)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	defer f.leave()

	if f.failGoals[goal] {
		http.Error(w, `{"error":{"message":"upstream exploded"}}`, http.StatusInternalServerError)
		return
	}

	args, _ := json.Marshal(map[string]string{"summary": "wrote " + goal})
	frame, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{
			"tool_calls": []any{map[string]any{
				"index": 0, "id": "c1", "type": "function",
				"function": map[string]any{"name": "finish", "arguments": string(args)},
			}},
		}}},
	})

	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
}

// goalFromRequest digs the task goal out of the prompt, which is how the fake
// tells the subtasks apart.
func goalFromRequest(body []byte) string {
	var req struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "?"
	}
	for _, m := range req.Messages {
		if _, after, found := strings.Cut(m.Content, "Task goal: "); found {
			line, _, _ := strings.Cut(after, "\n")
			return strings.TrimSpace(line)
		}
	}
	return "?"
}

func scheduledLoop(t *testing.T, f *fakeModel) (*Loop, *store.Store) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	s, err := store.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	client := openrouter.NewClient("test-key", "test-model", srv.URL)
	l := NewLoop(s, client, filepath.Join(dir, "output"))
	stopEverything(t, l)
	return l, s
}

// stopEverything makes a test wait for its runs before its directories go away.
// A test that leaves a schedule running — which several deliberately do — hands
// the temp directory to RemoveAll while a goroutine is still parked on the model
// call. Cancelling wakes it, and it writes its final status into a directory
// halfway through being deleted, which fails the cleanup rather than the test
// and points at whichever test happened to be running.
//
// It has to be registered after t.TempDir, since cleanups run last-registered
// first and this one must go before the removal.
func stopEverything(t *testing.T, l *Loop) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if !l.Shutdown(ctx) {
			t.Error("runs were still going when the test ended")
		}
	})
}

func TestRunGroupRespectsDependencyOrder(t *testing.T) {
	f := &fakeModel{}
	l, s := scheduledLoop(t, f)
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
	l.runGroup(context.Background(), plan)

	for title, id := range ids {
		task, err := s.GetTask(id)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if task.Status != models.StatusDone {
			t.Errorf("%s status = %q (%s), want %q", title, task.Status, task.Error, models.StatusDone)
		}
	}

	// A fully serial chain must arrive at the model in exactly that order, even
	// though the parallel budget was never the constraint.
	got := f.seen()
	want := []string{"schema", "impl", "tests"}
	if len(got) != len(want) {
		t.Fatalf("model saw %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("model saw %v, want %v", got, want)
		}
	}
}

func TestRunGroupRunsIndependentSubtasksAtOnce(t *testing.T) {
	// The delay makes overlap observable; without it three quick runs can
	// serialise by luck and the test would pass while proving nothing.
	f := &fakeModel{delay: 80 * time.Millisecond}
	l, s := scheduledLoop(t, f)
	groupID, _, ids := makeGroup(t, s, []struct {
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
	l.runGroup(context.Background(), plan)

	if peak := f.peakParallel(); peak < 2 {
		t.Errorf("peak parallel = %d, want the independent subtasks to overlap", peak)
	}
	for title, id := range ids {
		task, _ := s.GetTask(id)
		if task.Status != models.StatusDone {
			t.Errorf("%s status = %q, want done", title, task.Status)
		}
	}
}

func TestRunGroupHonoursParallelLimit(t *testing.T) {
	f := &fakeModel{delay: 60 * time.Millisecond}
	l, s := scheduledLoop(t, f)
	l.SetMaxParallel(2)

	groupID, _, _ := makeGroup(t, s, []struct {
		title  string
		writes []string
		reads  []string
	}{
		{"a", []string{"a.md"}, nil},
		{"b", []string{"b.md"}, nil},
		{"c", []string{"c.md"}, nil},
		{"d", []string{"d.md"}, nil},
	})

	plan, err := l.PlanGroup(groupID)
	if err != nil {
		t.Fatalf("PlanGroup: %v", err)
	}
	l.runGroup(context.Background(), plan)

	// The cap is the provider's rate limit standing in for itself; exceeding it
	// is the failure the whole setting exists to prevent.
	if peak := f.peakParallel(); peak > 2 {
		t.Errorf("peak parallel = %d, want at most 2", peak)
	}
	if len(f.seen()) != 4 {
		t.Errorf("model saw %d subtasks, want all 4 to have run", len(f.seen()))
	}
}

func TestRunGroupSkipsDependentsOfFailures(t *testing.T) {
	f := &fakeModel{failGoals: map[string]bool{"schema": true}}
	l, s := scheduledLoop(t, f)
	groupID, _, ids := makeGroup(t, s, []struct {
		title  string
		writes []string
		reads  []string
	}{
		{"schema", []string{"schema.json"}, nil},
		{"impl", []string{"board.js"}, []string{"schema.json"}},
		{"tests", []string{"board_test.js"}, []string{"board.js"}},
		{"unrelated", []string{"notes.md"}, nil},
	})

	plan, err := l.PlanGroup(groupID)
	if err != nil {
		t.Fatalf("PlanGroup: %v", err)
	}
	l.runGroup(context.Background(), plan)

	// Neither dependent may run: one reads the failed subtask's output directly,
	// the other reads the output of the one that was skipped. A cascade that
	// stops after a single level leaves an agent staring at a missing file.
	for _, title := range []string{"impl", "tests"} {
		task, err := s.GetTask(ids[title])
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if task.Status != models.StatusError {
			t.Errorf("%s status = %q, want %q", title, task.Status, models.StatusError)
		}
		if !strings.Contains(task.Error, "skipped") {
			t.Errorf("%s error = %q, want it marked skipped", title, task.Error)
		}
	}

	// They were skipped, not attempted — the whole point is not spending tokens
	// on work that cannot succeed.
	for _, goal := range f.seen() {
		if goal == "impl" || goal == "tests" {
			t.Errorf("subtask %q reached the model despite a failed dependency", goal)
		}
	}

	// A subtask reading nothing from the failure is unaffected and still runs.
	unrelated, _ := s.GetTask(ids["unrelated"])
	if unrelated.Status != models.StatusDone {
		t.Errorf("unrelated subtask status = %q (%s), want done", unrelated.Status, unrelated.Error)
	}
}

// Resuming a halted breakdown runs what is left of it. Re-running the subtasks
// that already filed their work spends the run again to reach the state it was
// already in — and for the anchor, reaches goal-met on a step it had reached
// before the plan was ever stopped.
func TestRunGroupResumesWithoutRepeatingFiledWork(t *testing.T) {
	f := &fakeModel{}
	l, s := scheduledLoop(t, f)
	groupID, _, ids := makeGroup(t, s, []struct {
		title  string
		writes []string
		reads  []string
	}{
		{"schema", []string{"schema.json"}, nil},
		{"impl", []string{"board.js"}, []string{"schema.json"}},
		{"integration", []string{"index.html"}, []string{"board.js"}},
	})
	// The state a stopped plan leaves behind: two subtasks in, the last one not.
	for _, title := range []string{"schema", "impl"} {
		if err := s.SetTaskInReview(ids[title], "wrote "+title); err != nil {
			t.Fatalf("SetTaskInReview: %v", err)
		}
	}

	plan, err := l.PlanGroup(groupID)
	if err != nil {
		t.Fatalf("PlanGroup: %v", err)
	}
	l.runGroup(context.Background(), plan)

	if got := f.seen(); len(got) != 1 || got[0] != "integration" {
		t.Errorf("model saw %v, want only the subtask that had not finished", got)
	}
	// And the one that had not finished did run, rather than being held behind
	// dependencies that settled in an earlier process.
	last, _ := s.GetTask(ids["integration"])
	if last.Status != models.StatusDone {
		t.Errorf("integration status = %q (%s), want done", last.Status, last.Error)
	}
}

func TestStartGroupRefusesSecondSchedule(t *testing.T) {
	f := &fakeModel{delay: 2 * time.Second}
	l, s := scheduledLoop(t, f)
	groupID, _, _ := makeGroup(t, s, []struct {
		title  string
		writes []string
		reads  []string
	}{
		{"a", []string{"a.md"}, nil},
		{"b", []string{"b.md"}, []string{"a.md"}},
	})

	if err := l.StartGroup(groupID); err != nil {
		t.Fatalf("StartGroup: %v", err)
	}
	defer l.StopGroup(groupID)

	if err := l.StartGroup(groupID); err != ErrGroupRunning {
		t.Errorf("second StartGroup returned %v, want ErrGroupRunning", err)
	}
}

func TestStopGroupCancelsRemainingSubtasks(t *testing.T) {
	f := &fakeModel{delay: 2 * time.Second}
	l, s := scheduledLoop(t, f)
	l.SetMaxParallel(1)

	groupID, _, ids := makeGroup(t, s, []struct {
		title  string
		writes []string
		reads  []string
	}{
		{"first", []string{"a.md"}, nil},
		{"second", []string{"b.md"}, []string{"a.md"}},
	})

	if err := l.StartGroup(groupID); err != nil {
		t.Fatalf("StartGroup: %v", err)
	}
	// Let the first subtask reach the model, then pull the schedule out.
	time.Sleep(150 * time.Millisecond)
	if !l.StopGroup(groupID) {
		t.Fatal("StopGroup reported no running schedule")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, _ := s.GetTask(ids["second"])
		if task.Status == models.StatusStopped {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	task, _ := s.GetTask(ids["second"])
	t.Errorf("downstream subtask status = %q, want %q after the group was stopped",
		task.Status, models.StatusStopped)
}
