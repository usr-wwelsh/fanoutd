package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fanoutd/internal/models"
)

// board is a fake fanoutd server covering the endpoints the CLI uses. It is
// enough to drive whole commands, which is where the exit codes live.
type board struct {
	mu    sync.Mutex
	tasks []models.Task
	trace map[string][]models.TraceStep
	files map[string][]models.FileEntry
	// statuses, when set for a task, are returned one per /status call, so a
	// watch can be walked through a run.
	statuses map[string][]models.Task
	// detached forces /status to report running=false even for a task whose
	// row says running — what a server restart leaves behind.
	detached map[string]bool
	token    string

	// breakdown is what POST /api/breakdown returns; groups is what the plan
	// endpoints serve, keyed by group id.
	breakdown *models.BreakdownResult
	groups    map[string]*models.GroupPlan
	// groupPlans, when set for a group, is served one entry per plan call, so a
	// group watch can be walked through a schedule.
	groupPlans map[string][]*models.GroupPlan
	stopped    []string
}

func newBoard() *board {
	return &board{
		trace:      map[string][]models.TraceStep{},
		files:      map[string][]models.FileEntry{},
		statuses:   map[string][]models.Task{},
		detached:   map[string]bool{},
		groups:     map[string]*models.GroupPlan{},
		groupPlans: map[string][]*models.GroupPlan{},
	}
}

func (b *board) task(id string) *models.Task {
	for i := range b.tasks {
		if b.tasks[i].ID == id {
			return &b.tasks[i]
		}
	}
	return nil
}

func (b *board) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.token != "" && r.Header.Get("Authorization") != "Bearer "+b.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 2 && parts[1] == "breakdown" {
		if b.breakdown == nil {
			http.Error(w, "no breakdown configured", http.StatusInternalServerError)
			return
		}
		b.tasks = append(b.tasks, b.breakdown.Tasks...)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(b.breakdown)
		return
	}
	if len(parts) >= 3 && parts[1] == "groups" {
		b.serveGroup(w, parts[2], action(parts, 3))
		return
	}
	if len(parts) == 2 && parts[1] == "tasks" {
		if r.Method == http.MethodPost {
			var nt models.Task
			json.NewDecoder(r.Body).Decode(&nt)
			nt.ID = "new00000-0000-0000-0000-00000000000f"
			nt.Column, nt.Status = "ideas", models.StatusIdle
			b.tasks = append(b.tasks, nt)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(nt)
			return
		}
		json.NewEncoder(w).Encode(b.tasks)
		return
	}
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}

	id := parts[2]
	task := b.task(id)
	if task == nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	switch action(parts, 3) {
	case "":
		json.NewEncoder(w).Encode(task)
	case "trace":
		json.NewEncoder(w).Encode(b.trace[id])
	case "files":
		json.NewEncoder(w).Encode(b.files[id])
	case "raw":
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("contents of " + r.URL.Query().Get("path")))
	case "status":
		if queue := b.statuses[id]; len(queue) > 0 {
			*task = queue[0]
			b.statuses[id] = queue[1:]
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status": task.Status, "running": task.Status == models.StatusRunning && !b.detached[id],
			"error": task.Error, "task": task,
		})
	case "start":
		task.Status = models.StatusRunning
		json.NewEncoder(w).Encode(map[string]any{"status": task.Status, "task": task})
	case "stop":
		task.Status = models.StatusStopped
		json.NewEncoder(w).Encode(map[string]any{"status": task.Status, "task": task})
	case "move":
		var req struct {
			Column string `json:"column"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		task.Column = req.Column
		json.NewEncoder(w).Encode(task)
	default:
		http.NotFound(w, r)
	}
}

func action(parts []string, at int) string {
	if len(parts) > at {
		return parts[at]
	}
	return ""
}

// serveGroup answers the plan endpoints. A queued sequence of plans is served
// one per call, which is how a group watch is driven through a schedule.
func (b *board) serveGroup(w http.ResponseWriter, id, act string) {
	plan := b.groups[id]
	if plan == nil {
		http.Error(w, "no such group", http.StatusNotFound)
		return
	}
	if act == "stop" {
		b.stopped = append(b.stopped, id)
		plan.Running = false
	}
	if queue := b.groupPlans[id]; len(queue) > 0 && act == "plan" {
		plan = queue[0]
		b.groups[id], b.groupPlans[id] = plan, queue[1:]
	}
	json.NewEncoder(w).Encode(plan)
}

// runCLI drives the real entry point against a fake server, which is what makes
// the exit codes part of the test rather than an implementation detail.
func runCLI(t *testing.T, b *board, args ...string) (int, string) {
	t.Helper()
	serve(t, b)
	var out bytes.Buffer
	code := run(args, &out)
	return code, out.String()
}

// serve points the CLI's configuration at a fake board and isolates it from the
// developer's own config file and environment.
func serve(t *testing.T, b *board) {
	t.Helper()
	srv := httptest.NewServer(b)
	t.Cleanup(srv.Close)
	t.Setenv("FANOUT_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))
	t.Setenv("FANOUT_URL", srv.URL)
	t.Setenv("FANOUT_TOKEN", b.token)
}

func boardWithTask(status string) *board {
	b := newBoard()
	b.tasks = []models.Task{{
		ID: "c762903a-1111-4444-8888-000000000001", Title: "Tetris clone",
		Column: "todo", Status: status, Goal: "build a tetris clone",
	}}
	return b
}

func TestLsShapesOutput(t *testing.T) {
	b := boardWithTask(models.StatusRunning)
	id := b.tasks[0].ID
	b.trace[id] = []models.TraceStep{
		{StepNumber: 1, Action: "planning"},
		// A verbatim response is exactly what must not reach the terminal.
		{StepNumber: 2, Action: "writing the page", Response: strings.Repeat("z", 4000),
			ToolName: "write_file", ToolResult: "wrote tetris.html (4210 bytes)"},
	}
	b.files[id] = []models.FileEntry{{Path: "tetris.html", Size: 4210}}

	code, out := runCLI(t, b, "ls")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if strings.Contains(out, "zzzz") {
		t.Error("ls leaked a verbatim response")
	}
	for _, want := range []string{"c762903", "Tetris clone", "todo", "running", "step 2", "write_file"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls output missing %q:\n%s", want, out)
		}
	}
	if lines := strings.Count(strings.TrimRight(out, "\n"), "\n") + 1; lines != 1 {
		t.Errorf("want one line per task, got %d:\n%s", lines, out)
	}
}

func TestLsFilters(t *testing.T) {
	b := boardWithTask(models.StatusRunning)
	b.tasks = append(b.tasks, models.Task{ID: "88d9af4b-0000-0000-0000-000000000002",
		Title: "Research digest MVP", Column: "finished", Status: models.StatusDone})

	code, out := runCLI(t, b, "ls", "--col", "finished")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if strings.Contains(out, "Tetris") || !strings.Contains(out, "Research digest") {
		t.Errorf("filter did not apply:\n%s", out)
	}
}

func TestLsJSON(t *testing.T) {
	b := boardWithTask(models.StatusDone)
	code, out := runCLI(t, b, "ls", "--json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	var rows []taskRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("--json is not machine-readable: %v\n%s", err, out)
	}
	if len(rows) != 1 || rows[0].Task.Title != "Tetris clone" {
		t.Errorf("got %+v", rows)
	}
}

func TestPrefixResolutionAcrossCommands(t *testing.T) {
	b := boardWithTask(models.StatusIdle)
	code, out := runCLI(t, b, "start", "c762")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "running") {
		t.Errorf("got %q", out)
	}
}

func TestUnknownTaskFails(t *testing.T) {
	b := boardWithTask(models.StatusIdle)
	if code, _ := runCLI(t, b, "show", "zzzz"); code != exitFailure {
		t.Errorf("exit %d, want %d", code, exitFailure)
	}
}

// The exit codes are the reason this composes with shell scripts, so they are
// pinned here rather than left to the caller to discover.
func TestExitCodeForFailedTask(t *testing.T) {
	b := boardWithTask(models.StatusError)
	b.tasks[0].Error = "reached the 20 step limit without meeting the goal"

	code, out := runCLI(t, b, "show", "c762")
	if code != exitTaskError {
		t.Fatalf("exit %d, want %d\n%s", code, exitTaskError, out)
	}
	if !strings.Contains(out, "20 step limit") {
		t.Errorf("the reason should be printed:\n%s", out)
	}
}

func TestWatchFollowsToCompletion(t *testing.T) {
	b := boardWithTask(models.StatusRunning)
	id := b.tasks[0].ID
	b.trace[id] = []models.TraceStep{{StepNumber: 1, Action: "planning", ToolName: "list_files"}}
	b.statuses[id] = []models.Task{
		{ID: id, Title: "Tetris clone", Status: models.StatusRunning},
		{ID: id, Title: "Tetris clone", Status: models.StatusDone, Summary: "wrote tetris.html"},
	}

	code, out := runCLI(t, b, "watch", "c762", "--interval", "1ms")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "wrote tetris.html") {
		t.Errorf("want the summary on completion:\n%s", out)
	}
	if !strings.Contains(out, "planning") {
		t.Errorf("want steps printed as they land:\n%s", out)
	}
	if strings.Count(out, "planning") != 1 {
		t.Errorf("a step should print once, not on every poll:\n%s", out)
	}
}

func TestWatchExitsNonZeroOnTaskError(t *testing.T) {
	b := boardWithTask(models.StatusRunning)
	id := b.tasks[0].ID
	b.statuses[id] = []models.Task{{ID: id, Status: models.StatusError, Error: "model call failed"}}

	code, out := runCLI(t, b, "watch", "c762", "--interval", "1ms")
	if code != exitTaskError {
		t.Fatalf("exit %d, want %d\n%s", code, exitTaskError, out)
	}
	if !strings.Contains(out, "model call failed") {
		t.Errorf("got %q", out)
	}
}

// A row that says running with no loop attached never resolves. Watch must not
// poll it forever.
func TestWatchDoesNotHangOnDetachedRun(t *testing.T) {
	b := boardWithTask(models.StatusRunning)
	b.detached[b.tasks[0].ID] = true
	serve(t, b)

	type result struct {
		code int
		out  string
	}
	done := make(chan result, 1)
	go func() {
		var out bytes.Buffer
		code := run([]string{"watch", "c762", "--interval", "1ms"}, &out)
		done <- result{code, out.String()}
	}()

	select {
	case got := <-done:
		if got.code != exitTaskError {
			t.Fatalf("exit %d, want %d\n%s", got.code, exitTaskError, got.out)
		}
		if !strings.Contains(got.out, "no longer attached") {
			t.Errorf("want an explanation of the stale row:\n%s", got.out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch hung on a detached run")
	}
}

func TestWatchIdleTaskReturnsImmediately(t *testing.T) {
	b := boardWithTask(models.StatusIdle)
	code, out := runCLI(t, b, "watch", "c762", "--interval", "1ms")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "idle") {
		t.Errorf("got %q", out)
	}
}

func TestTraceIsTruncatedUnlessFull(t *testing.T) {
	b := boardWithTask(models.StatusDone)
	id := b.tasks[0].ID
	b.trace[id] = []models.TraceStep{{
		StepNumber: 1, Action: "writing", Response: "VERBATIM-RESPONSE",
		ToolName: "write_file", ToolResult: "wrote a.html (10 bytes)",
	}}

	code, out := runCLI(t, b, "trace", "c762")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if strings.Contains(out, "VERBATIM-RESPONSE") {
		t.Errorf("default trace must not dump responses:\n%s", out)
	}

	code, full := runCLI(t, b, "trace", "c762", "--full")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, full)
	}
	if !strings.Contains(full, "VERBATIM-RESPONSE") {
		t.Errorf("--full must dump responses:\n%s", full)
	}
}

func TestCatResolvesByBasename(t *testing.T) {
	b := boardWithTask(models.StatusDone)
	b.files[b.tasks[0].ID] = []models.FileEntry{{Path: "src/tetris.html", Size: 10}}

	code, out := runCLI(t, b, "cat", "c762", "tetris.html")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if out != "contents of src/tetris.html" {
		t.Errorf("got %q", out)
	}
}

func TestAddRequiresGoalToStart(t *testing.T) {
	b := newBoard()
	code, _ := runCLI(t, b, "add", "an idea", "--start")
	if code != exitFailure {
		t.Errorf("exit %d, want %d", code, exitFailure)
	}
}

func TestAddCreatesAndStarts(t *testing.T) {
	b := newBoard()
	code, out := runCLI(t, b, "add", "Tetris", "clone", "--goal", "build it", "--start")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if len(b.tasks) != 1 || b.tasks[0].Title != "Tetris clone" {
		t.Fatalf("got %+v", b.tasks)
	}
	if b.tasks[0].Goal != "build it" {
		t.Errorf("goal = %q", b.tasks[0].Goal)
	}
	if b.tasks[0].Status != models.StatusRunning {
		t.Errorf("--start did not start it: %s", b.tasks[0].Status)
	}
}

func TestUnauthorizedExplainsTheToken(t *testing.T) {
	b := boardWithTask(models.StatusIdle)
	b.token = "secret"

	srv := httptest.NewServer(b)
	t.Cleanup(srv.Close)
	t.Setenv("FANOUT_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))
	t.Setenv("FANOUT_URL", srv.URL)
	t.Setenv("FANOUT_TOKEN", "")

	var out bytes.Buffer
	if code := run([]string{"ls"}, &out); code != exitFailure {
		t.Errorf("exit %d, want %d", code, exitFailure)
	}
}

func TestUnknownCommand(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"frobnicate"}, &out); code != exitFailure {
		t.Errorf("exit %d, want %d", code, exitFailure)
	}
}

func TestHelpExitsZero(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"help"}, &out); code != exitOK {
		t.Errorf("exit %d", code)
	}
}

func TestNoArgsIsAFailure(t *testing.T) {
	var out bytes.Buffer
	if code := run(nil, &out); code != exitFailure {
		t.Errorf("exit %d", code)
	}
}
