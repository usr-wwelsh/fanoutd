package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fanoutd/internal/models"
	"fanoutd/internal/openrouter"
	"fanoutd/internal/store"
)

// fakeReviewer answers the review pass from a queue of canned calls and hands
// everything else to the author fake, so one server covers a run and the verdict
// on it. Telling the two apart by the system prompt is what a real provider sees
// too: the reviewer is the same endpoint with a different brief.
type fakeReviewer struct {
	author *fakeModel

	mu    sync.Mutex
	calls []reviewCall
	// briefs holds the opening prompt of every review pass, which is what a test
	// about what a reviewer was told has to look at.
	briefs []string
}

func (f *fakeReviewer) lastBrief() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.briefs) == 0 {
		return ""
	}
	return f.briefs[len(f.briefs)-1]
}

// reviewCall is one tool call the reviewer should make. Anything other than pass
// or reject is an ordinary step, which is how a reviewer that reaches for a write
// tool is expressed.
type reviewCall struct {
	name string
	args map[string]string
}

func (f *fakeReviewer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if !isReviewRequest(body) {
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		f.author.ServeHTTP(w, r)
		return
	}

	f.mu.Lock()
	call := reviewCall{name: passTool, args: map[string]string{"summary": "nothing left to check"}}
	if len(f.calls) > 0 {
		call, f.calls = f.calls[0], f.calls[1:]
	}
	f.briefs = append(f.briefs, reviewBrief(body))
	f.mu.Unlock()

	args, _ := json.Marshal(call.args)
	frame, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{
			"tool_calls": []any{map[string]any{
				"index": 0, "id": "r1", "type": "function",
				"function": map[string]any{"name": call.name, "arguments": string(args)},
			}},
		}}},
	})
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
}

func isReviewRequest(body []byte) bool {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return false
	}
	return req.Messages[0].Role == "system" &&
		strings.Contains(req.Messages[0].Content, "You are reviewing work produced by another agent")
}

// reviewBrief is the user turn a review pass opens with: the goal, the criteria
// and the file listing the reviewer was handed.
func reviewBrief(body []byte) string {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) < 2 {
		return ""
	}
	return req.Messages[1].Content
}

func reviewLoop(t *testing.T, calls ...reviewCall) (*Loop, *store.Store) {
	t.Helper()
	l, s, _ := reviewLoopWithModel(t, calls...)
	return l, s
}

// reviewLoopWithModel is reviewLoop for a test that also has to ask what the
// reviewer was told.
func reviewLoopWithModel(t *testing.T, calls ...reviewCall) (*Loop, *store.Store, *fakeReviewer) {
	t.Helper()
	f := &fakeReviewer{author: &fakeModel{}, calls: calls}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	s, err := store.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	client := openrouter.NewClient("test-key", "test-model", srv.URL)
	l := NewLoop(s, client, filepath.Join(dir, "output"))
	l.SetReview(true, "")
	stopEverything(t, l)
	return l, s, f
}

// awaiting creates a task in the state a finished run leaves behind: parked in
// the review column, done, with the summary its author wrote.
func awaiting(t *testing.T, s *store.Store, nt store.NewTask, summary string) models.Task {
	t.Helper()
	if nt.Title == "" {
		nt.Title = "build the thing"
	}
	if nt.Goal == "" {
		nt.Goal = "build the thing"
	}
	task, err := s.CreateTaskFrom(nt)
	if err != nil {
		t.Fatalf("CreateTaskFrom: %v", err)
	}
	if err := s.SetTaskInReview(task.ID, summary); err != nil {
		t.Fatalf("SetTaskInReview: %v", err)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return *got
}

func TestReviewPassFilesTheWork(t *testing.T) {
	l, s := reviewLoop(t, reviewCall{passTool, map[string]string{"summary": "ran the tests, all four criteria hold"}})
	task := awaiting(t, s, store.NewTask{Criteria: "it builds"}, "wrote main.go")

	l.reviewAfterRun(context.Background(), task.ID)

	got, _ := s.GetTask(task.ID)
	if got.Column != models.ColumnFinished {
		t.Errorf("column = %q, want %q", got.Column, models.ColumnFinished)
	}
	if got.Status != models.StatusDone {
		t.Errorf("status = %q, want %q", got.Status, models.StatusDone)
	}
	// Both halves survive: what the author claimed, and what somebody else made
	// of it. Overwriting the first with the second loses the claim being checked.
	for _, want := range []string{"wrote main.go", "all four criteria hold"} {
		if !strings.Contains(got.Summary, want) {
			t.Errorf("summary %q does not carry %q", got.Summary, want)
		}
	}
}

func TestReviewRejectionOpensAReworkTask(t *testing.T) {
	const findings = "main.go does not compile: undefined mount at line 12"
	l, s := reviewLoop(t, reviewCall{rejectTool, map[string]string{"findings": findings}})
	task := awaiting(t, s, store.NewTask{Criteria: "it builds\nit runs"}, "wrote main.go")

	l.reviewAfterRun(context.Background(), task.ID)

	// The reviewed task is not moved on. It was neither accepted nor is it the
	// thing that will be fixed; the rework task is.
	got, _ := s.GetTask(task.ID)
	if got.Column != models.ColumnReview {
		t.Errorf("reviewed task column = %q, want it left in %q", got.Column, models.ColumnReview)
	}
	if !strings.Contains(got.Summary, findings) {
		t.Errorf("summary %q does not record why it was sent back", got.Summary)
	}

	rework := reworkOf(t, s, task.ID)
	if rework.Goal != findings {
		t.Errorf("rework goal = %q, want the findings verbatim", rework.Goal)
	}
	if rework.Criteria != task.Criteria {
		t.Errorf("rework criteria = %q, want the original %q", rework.Criteria, task.Criteria)
	}
	// Same workspace, so the rework repairs the work rather than starting over.
	if rework.WorkspaceID != task.WorkspaceID {
		t.Errorf("rework workspace = %q, want the reviewed task's %q", rework.WorkspaceID, task.WorkspaceID)
	}
	if rework.ReviewRound != 1 {
		t.Errorf("rework round = %d, want 1", rework.ReviewRound)
	}
}

// A rework exists because a review rejected something, and that something is
// still parked in the review column. Nothing else ever looks at it again, so a
// rework that passes has to file the work it repaired along with itself.
func TestReworkVerdictFilesTheWorkItRepaired(t *testing.T) {
	l, s, f := reviewLoopWithModel(t, reviewCall{passTool, map[string]string{"summary": "the href resolves now"}})
	original := awaiting(t, s, store.NewTask{
		Title: "Index Page", Goal: "build the page", Criteria: "the stylesheet loads",
	}, "wrote index.html")
	rework := awaiting(t, s, store.NewTask{
		Title:       "Index Page — rework 1",
		Goal:        `href="src/styles.css" resolves to src/src/styles.css`,
		Criteria:    original.Criteria,
		ReviewRound: 1,
		ParentID:    original.ID,
		WorkspaceID: original.WorkspaceID,
	}, "fixed the href")

	l.reviewAfterRun(context.Background(), rework.ID)

	for _, id := range []string{rework.ID, original.ID} {
		got, _ := s.GetTask(id)
		if got.Column != models.ColumnFinished {
			t.Errorf("task %q column = %q, want %q", got.Title, got.Column, models.ColumnFinished)
		}
	}
	// Held to what the work was for, not to the wording of the fault it repairs.
	// Otherwise a rework passes having fixed the one line named and broken
	// everything around it.
	if brief := f.lastBrief(); !strings.Contains(brief, "build the page") {
		t.Errorf("the reviewer was given %q, want the original goal", brief)
	}
}

// A rejected subtask's whole group waits in the review column, since a breakdown
// is judged as one thing. The rework's verdict is the one that releases them.
func TestReworkVerdictFilesTheGroupItRepaired(t *testing.T) {
	l, s := reviewLoop(t, reviewCall{passTool, map[string]string{"summary": "the pieces fit"}})
	_, workspace, byTitle := makeGroup(t, s, []struct {
		title  string
		writes []string
		reads  []string
	}{
		{"first", []string{"a.md"}, nil},
		{"second", []string{"b.md"}, []string{"a.md"}},
	})
	for _, id := range byTitle {
		if err := s.SetTaskInReview(id, "wrote my part"); err != nil {
			t.Fatalf("SetTaskInReview: %v", err)
		}
	}
	rework := awaiting(t, s, store.NewTask{
		Title:       "second — rework 1",
		Goal:        "b.md contradicts a.md",
		ReviewRound: 1,
		ParentID:    byTitle["second"],
		WorkspaceID: workspace,
	}, "reconciled the two")

	l.reviewAfterRun(context.Background(), rework.ID)

	for title, id := range byTitle {
		got, _ := s.GetTask(id)
		if got.Column != models.ColumnFinished {
			t.Errorf("subtask %q column = %q, want the rework's verdict to file the whole group", title, got.Column)
		}
	}
	if got, _ := s.GetTask(rework.ID); got.Column != models.ColumnFinished {
		t.Errorf("rework column = %q, want %q", got.Column, models.ColumnFinished)
	}
}

// The round limit ends the chain, and it has to end it for everything the chain
// was about — work left filed as done in the review column is work no listing
// shows and nobody is asked about.
func TestReworkAtTheRoundLimitBlocksWhatItCovers(t *testing.T) {
	l, s := reviewLoop(t, reviewCall{rejectTool, map[string]string{"findings": "still broken"}})
	original := awaiting(t, s, store.NewTask{Title: "Index Page"}, "wrote index.html")
	rework := awaiting(t, s, store.NewTask{
		Title:       "Index Page — rework 2",
		ReviewRound: maxReviewRounds,
		ParentID:    original.ID,
		WorkspaceID: original.WorkspaceID,
	}, "tried again")

	l.reviewAfterRun(context.Background(), rework.ID)

	for _, id := range []string{rework.ID, original.ID} {
		got, _ := s.GetTask(id)
		if got.Status != models.StatusError {
			t.Errorf("task %q status = %q, want %q so `fanout blocked` lists it", got.Title, got.Status, models.StatusError)
		}
	}
}

// A verdict is delivered by the goroutine that ran the task, so a process that
// goes away between a run settling and its review finishing leaves work parked
// with nothing left that would ever look at it again.
func TestReviewParkedSweepsWorkLeftBehind(t *testing.T) {
	l, s := reviewLoop(t, reviewCall{passTool, map[string]string{"summary": "checked it"}})
	task := awaiting(t, s, store.NewTask{Criteria: "it builds"}, "wrote main.go")

	if n := l.ReviewParked(context.Background()); n != 1 {
		t.Fatalf("ReviewParked queued %d verdict(s), want 1", n)
	}
	waitForColumn(t, s, task.ID, models.ColumnFinished)
}

// The sweep answers per verdict, not per task: a breakdown is one call, and a
// rejected task with a rework under it is covered by the rework's.
func TestParkedTargetsCollapseToOneVerdictEach(t *testing.T) {
	l, s := reviewLoop(t)
	_, _, byTitle := makeGroup(t, s, []struct {
		title  string
		writes []string
		reads  []string
	}{
		{"first", []string{"a.md"}, nil},
		{"second", []string{"b.md"}, []string{"a.md"}},
	})
	for _, id := range byTitle {
		if err := s.SetTaskInReview(id, "wrote my part"); err != nil {
			t.Fatalf("SetTaskInReview: %v", err)
		}
	}
	solo := awaiting(t, s, store.NewTask{Title: "solo"}, "wrote it")
	rework := awaiting(t, s, store.NewTask{
		Title: "solo — rework 1", ReviewRound: 1, ParentID: solo.ID, WorkspaceID: solo.WorkspaceID,
	}, "fixed it")

	targets, err := l.parkedTargets()
	if err != nil {
		t.Fatalf("parkedTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("parkedTargets returned %d target(s), want one per verdict owed", len(targets))
	}

	covered := map[string]int{}
	for _, target := range targets {
		for _, task := range target.covers {
			covered[task.ID]++
		}
	}
	for _, id := range []string{solo.ID, rework.ID, byTitle["first"], byTitle["second"]} {
		if covered[id] != 1 {
			t.Errorf("task %s covered by %d verdicts, want exactly 1", shortID(id), covered[id])
		}
	}
}

// A task the server has since started is not parked any more, and the run that
// started it will review it when it ends. Two agents in one workspace is the
// thing the claim prevents.
func TestReviewParkedSkipsATaskAlreadyBusy(t *testing.T) {
	l, s := reviewLoop(t, reviewCall{passTool, map[string]string{"summary": "checked it"}})
	task := awaiting(t, s, store.NewTask{}, "wrote main.go")

	if _, ok := l.claimRun(context.Background(), task.ID); !ok {
		t.Fatal("could not claim the task")
	}
	defer l.clearRun(task.ID)

	l.ReviewParked(context.Background())

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got, _ := s.GetTask(task.ID); got.Column != models.ColumnReview {
			t.Fatalf("column = %q, want the busy task left alone", got.Column)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForColumn(t *testing.T, s *store.Store, taskID, column string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := s.GetTask(taskID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.Column == column {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := s.GetTask(taskID)
	t.Fatalf("column = %q, want %q", got.Column, column)
}

// The bound. Without it a task the model cannot fix bounces between todo and
// review against a metered endpoint for as long as the server is up.
func TestReviewStopsAtTheRoundLimit(t *testing.T) {
	l, s := reviewLoop(t, reviewCall{rejectTool, map[string]string{"findings": "still broken"}})
	task := awaiting(t, s, store.NewTask{ReviewRound: maxReviewRounds}, "wrote main.go")

	l.reviewAfterRun(context.Background(), task.ID)

	if other := findRework(t, s, task.ID); other != nil {
		t.Fatalf("a %dth rework was opened past the limit of %d", other.ReviewRound, maxReviewRounds)
	}
	got, _ := s.GetTask(task.ID)
	// Error is what puts it in front of a person: `fanout blocked` lists the
	// review column for exactly this.
	if got.Status != models.StatusError {
		t.Errorf("status = %q, want %q so it surfaces as blocked", got.Status, models.StatusError)
	}
}

// Advertising a narrower tool set is not enough on its own: a model that has
// seen write_file in another life will still emit one, and Workspace.Exec would
// carry it out.
func TestReviewCannotChangeTheWork(t *testing.T) {
	l, s := reviewLoop(t,
		reviewCall{"write_file", map[string]string{"path": "sneaked.txt", "content": "x"}},
		reviewCall{passTool, map[string]string{"summary": "looks right"}},
	)
	task := awaiting(t, s, store.NewTask{Criteria: "it builds"}, "wrote main.go")

	ws, err := l.Workspace(task.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if err := os.MkdirAll(ws.Root(), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Root(), "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	l.reviewAfterRun(context.Background(), task.ID)

	if _, err := os.Stat(filepath.Join(ws.Root(), "sneaked.txt")); !os.IsNotExist(err) {
		t.Fatal("the reviewer wrote a file into the workspace it was judging")
	}
	// Refused, not aborted: the reviewer is told why and goes on to a verdict.
	if got, _ := s.GetTask(task.ID); got.Column != models.ColumnFinished {
		t.Errorf("column = %q, want the refusal to leave the pass that followed intact", got.Column)
	}
}

// A subtask cannot be reviewed on its own: sending one back would invalidate
// every sibling that already read its output, and nothing tracks a stale read.
// So a subtask that finishes while a sibling is still out waits for it.
func TestReviewLeavesSubtasksToTheirGroup(t *testing.T) {
	l, s := reviewLoop(t, reviewCall{rejectTool, map[string]string{"findings": "broken"}})
	task := awaiting(t, s, store.NewTask{GroupID: "g1", WorkspaceID: "ws1"}, "wrote a part")
	if _, err := s.CreateTaskFrom(store.NewTask{Title: "the other part", GroupID: "g1", WorkspaceID: "ws1"}); err != nil {
		t.Fatalf("CreateTaskFrom: %v", err)
	}

	l.reviewAfterRun(context.Background(), task.ID)

	got, _ := s.GetTask(task.ID)
	if got.Status != models.StatusDone {
		t.Errorf("status = %q, want the subtask untouched until its group settles", got.Status)
	}
	if other := findRework(t, s, task.ID); other != nil {
		t.Error("a subtask was sent back on its own")
	}
}

// The last subtask of a halted breakdown is ordinarily restarted by hand, and
// the schedule goroutine that would have reviewed the group is gone by then.
// The run that completes the group asks for the verdict instead, or the work
// sits in the review column with nothing left that would ever judge it.
func TestSubtaskFinishedAloneReviewsItsGroup(t *testing.T) {
	l, s := reviewLoop(t, reviewCall{rejectTool, map[string]string{"findings": "index.html is empty"}})
	first := awaiting(t, s, store.NewTask{Title: "stylesheet", GroupID: "g1", WorkspaceID: "ws1"}, "wrote style.css")
	last := awaiting(t, s, store.NewTask{Title: "integration", GroupID: "g1", WorkspaceID: "ws1"}, "wrote index.html")

	l.reviewAfterRun(context.Background(), last.ID)

	rework := findRework(t, s, last.ID)
	if rework == nil {
		t.Fatal("the group was never reviewed once its last subtask came in")
	}
	// One verdict over the whole breakdown, not one per subtask.
	for _, id := range []string{first.ID, last.ID} {
		got, _ := s.GetTask(id)
		if got.Verdict != models.VerdictRejected {
			t.Errorf("task %s verdict = %q, want the group's", got.Title, got.Verdict)
		}
	}
}

// A verdict is work being done to the group, so a start arriving while it runs
// must not re-run the subtasks the reviewer is reading.
func TestGroupUnderReviewRefusesAStart(t *testing.T) {
	l, s := reviewLoop(t)
	awaiting(t, s, store.NewTask{GroupID: "g1"}, "wrote it")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, ok := l.claimGroup(ctx, "g1"); !ok {
		t.Fatal("could not claim an idle group")
	}
	if err := l.StartGroup("g1"); err != ErrGroupRunning {
		t.Errorf("StartGroup = %v, want %v", err, ErrGroupRunning)
	}
	l.clearGroup("g1")
}

// The anchor is where a group's verdict and its rework hang. The schedule
// already puts the subtask that reads everyone else's output last, so this needs
// no flag of its own.
func TestAnchorIsTheLastSubtaskScheduled(t *testing.T) {
	tasks := []models.Task{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	plan := &Plan{Waves: [][]string{{"a", "b"}, {"c"}}}

	got := anchorTask(plan, tasks)
	if got == nil || got.ID != "c" {
		t.Fatalf("anchor = %v, want the last subtask of the last wave", got)
	}
	if anchorTask(&Plan{}, tasks) != nil {
		t.Error("a plan with no waves produced an anchor")
	}
}

func TestSplitVerdictPrefersReject(t *testing.T) {
	verdict, note, rest := splitVerdict([]pendingCall{
		{Call: ToolCall{Name: "read_file", Path: "main.go"}},
		{Call: ToolCall{Name: passTool, Summary: "fine"}},
		{Call: ToolCall{Name: rejectTool, Findings: "not fine"}},
	})
	// A model that emitted both found something, and filing that as accepted is
	// the one mistake here that nobody sees.
	if verdict != rejectTool || note != "not fine" {
		t.Errorf("verdict = (%q, %q), want reject", verdict, note)
	}
	// The ordinary call beside it still has to run, or the transcript is left
	// holding a call id nothing answered.
	if len(rest) != 1 || rest[0].Call.Name != "read_file" {
		t.Errorf("rest = %v, want the read kept", rest)
	}
}

// Recording the verdict on the task is what makes it visible where the work is.
// Replaying it to the author is a different thing entirely: the model reads a
// critique of its own output as a turn it made itself, and argues with it.
func TestAuthorReplaySkipsReviewSteps(t *testing.T) {
	trace := []models.TraceStep{
		{Action: "wrote main.go", Response: "writing the entry point"},
		{Action: reviewPrefix + "rejected", ToolResult: "main.go does not compile"},
	}
	if got := authorSteps(trace); len(got) != 1 || got[0].Action != "wrote main.go" {
		t.Fatalf("authorSteps kept %d step(s), want only the author's", len(got))
	}

	task := &models.Task{Goal: "build it", Criteria: "it builds"}
	msgs := buildMessages(task, "/tmp/ws", nil, trace, false)
	for _, m := range msgs {
		if strings.Contains(m.Content, "does not compile") {
			t.Error("a review finding reached the author's own transcript")
		}
	}
	// The criteria do reach it, though: an agent that does not know what it will
	// be held to can only find out at review time.
	if !strings.Contains(msgs[1].Content, "it builds") {
		t.Error("the criteria were withheld from the author")
	}
}

// With review off, a run goes from todo straight to finished exactly as before.
func TestSettleGoesStraightToFinishedWithoutReview(t *testing.T) {
	_, s := reviewLoop(t)
	dir := t.TempDir()
	client := openrouter.NewClient("test-key", "test-model", "http://127.0.0.1:1")
	plain := NewLoop(s, client, filepath.Join(dir, "output"))

	task, err := s.CreateTaskFrom(store.NewTask{Title: "t", Goal: "g"})
	if err != nil {
		t.Fatalf("CreateTaskFrom: %v", err)
	}
	if err := plain.settleRun(task.ID, "done"); err != nil {
		t.Fatalf("settleRun: %v", err)
	}
	if got, _ := s.GetTask(task.ID); got.Column != models.ColumnFinished {
		t.Errorf("column = %q, want %q with review off", got.Column, models.ColumnFinished)
	}
}

// reworkOf is findRework with the absence treated as a failure.
func reworkOf(t *testing.T, s *store.Store, parentID string) models.Task {
	t.Helper()
	got := findRework(t, s, parentID)
	if got == nil {
		t.Fatal("no rework task was opened")
	}
	return *got
}

func findRework(t *testing.T, s *store.Store, parentID string) *models.Task {
	t.Helper()
	tasks, err := s.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	for i := range tasks {
		if tasks[i].ParentID == parentID {
			return &tasks[i]
		}
	}
	return nil
}
