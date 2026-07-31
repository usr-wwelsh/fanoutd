package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fanoutd/internal/models"
	"fanoutd/internal/store"
)

func testLoop(t *testing.T) (*Loop, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return NewLoop(s, nil, filepath.Join(dir, "output")), s
}

// writeWorkspaceFile puts a file where the task's workspace will look for it.
func writeWorkspaceFile(t *testing.T, l *Loop, taskID, name, content string) {
	t.Helper()
	ws, err := l.Workspace(taskID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if err := os.MkdirAll(ws.Root(), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Root(), name), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// The case this exists for: the agent wrote a working file, then burned its
// remaining steps re-reading it instead of calling finish.
func TestConcedeFinishesWhenWorkspaceHasFiles(t *testing.T) {
	l, s := testLoop(t)
	task, err := s.CreateTask("spreadsheet", "", "build it", "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	writeWorkspaceFile(t, l, task.ID, "index.html", "<!DOCTYPE html>")

	l.concede(task.ID, 14, "agent repeated the same read_file call 3 times without making progress")

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != models.StatusDone {
		t.Errorf("status = %q, want %q", got.Status, models.StatusDone)
	}
	if got.Error != "" {
		t.Errorf("error = %q, want it cleared", got.Error)
	}
	if !strings.Contains(got.Summary, "index.html") {
		t.Errorf("summary %q should name the file that was produced", got.Summary)
	}
	// The summary must not read like the model signed the work off itself.
	if !strings.Contains(got.Summary, "without calling finish") {
		t.Errorf("summary %q should say finish was never called", got.Summary)
	}
	if !strings.Contains(got.Summary, "read_file") {
		t.Errorf("summary %q should carry the reason the run stopped", got.Summary)
	}
}

// A run that produced nothing really did fail, and must still say so — otherwise
// the guard becomes a way to launder a dead run into a green one.
func TestConcedeFailsWhenWorkspaceEmpty(t *testing.T) {
	l, s := testLoop(t)
	task, err := s.CreateTask("spreadsheet", "", "build it", "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	l.concede(task.ID, 3, "model returned an unusable response 3 times in a row")

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != models.StatusError {
		t.Errorf("status = %q, want %q", got.Status, models.StatusError)
	}
	if !strings.Contains(got.Error, "unusable response") {
		t.Errorf("error = %q, want the reason preserved", got.Error)
	}
}

// Subtasks of one breakdown share a workspace, so a listing of it is not
// evidence that any particular one did anything. A subtask that wrote nothing
// must fail even while its siblings' files sit next to it, or a whole group
// reports green off the work of its one productive member.
func TestConcedeFailsWhenOnlyASiblingProduced(t *testing.T) {
	l, s := testLoop(t)
	const group, workspace = "group-1", "workspace-1"

	worker, err := s.CreateTaskFrom(store.NewTask{
		Title: "plasma demo", Goal: "write it", GroupID: group, WorkspaceID: workspace,
	})
	if err != nil {
		t.Fatalf("CreateTaskFrom: %v", err)
	}
	idler, err := s.CreateTaskFrom(store.NewTask{
		Title: "particle demo", Goal: "write it", GroupID: group, WorkspaceID: workspace,
	})
	if err != nil {
		t.Fatalf("CreateTaskFrom: %v", err)
	}

	// Only the worker writes, and only the worker claims what it wrote.
	writeWorkspaceFile(t, l, worker.ID, "plasma.js", "export function init() {}")
	if owner, err := s.ClaimWrite(workspace, "plasma.js", worker.ID); err != nil || owner != "" {
		t.Fatalf("ClaimWrite: owner=%q err=%v", owner, err)
	}

	l.concede(idler.ID, 3, "agent repeated the same list_files call 3 times without making progress")

	got, err := s.GetTask(idler.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != models.StatusError {
		t.Errorf("status = %q, want %q — a sibling's file is not this task's output", got.Status, models.StatusError)
	}
	if strings.Contains(got.Summary, "plasma.js") {
		t.Errorf("summary %q credits the task with a file it did not write", got.Summary)
	}

	// The sibling that did the work still concedes to done, on its own file.
	l.concede(worker.ID, 5, "reached the 20 step limit without meeting the goal")
	done, err := s.GetTask(worker.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if done.Status != models.StatusDone {
		t.Errorf("worker status = %q, want %q", done.Status, models.StatusDone)
	}
	if !strings.Contains(done.Summary, "plasma.js") {
		t.Errorf("worker summary %q should name the file it wrote", done.Summary)
	}
}

func TestConcededSummaryPluralises(t *testing.T) {
	one := concededSummary([]FileEntry{{Path: "a.html"}}, 12, "step limit")
	if !strings.Contains(one, "1 file:") {
		t.Errorf("single file summary = %q", one)
	}
	two := concededSummary([]FileEntry{{Path: "a.html"}, {Path: "b.js"}}, 12, "step limit")
	if !strings.Contains(two, "2 files:") {
		t.Errorf("multi file summary = %q", two)
	}
	if !strings.Contains(two, "a.html, b.js") {
		t.Errorf("multi file summary should list both: %q", two)
	}
}

// A conceded run is filed as finished so its deliverables are not buried behind
// a red status, and that mark is also the loop's stop signal. Starting the task
// again has to withdraw it, or the resume returns on its first step and leaves
// the task running until a server restart reclaims it.
func TestConcededTaskCanBeResumed(t *testing.T) {
	l, s := testLoop(t)
	task, err := s.CreateTask("tool", "", "build it", "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	writeWorkspaceFile(t, l, task.ID, "main.go", "package main")

	l.concede(task.ID, 19, "agent repeated the same run_command call 3 times without making progress")

	conceded, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !conceded.FinishFlag {
		t.Fatal("conceding should file the task as finished")
	}

	// Start would run the agent, so exercise what it does to the row.
	if err := s.ClearFinishFlag(task.ID); err != nil {
		t.Fatalf("ClearFinishFlag: %v", err)
	}
	resumed, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if resumed.FinishFlag {
		t.Fatal("starting a conceded task must withdraw the finished mark")
	}
	if resumed.Summary != conceded.Summary {
		t.Errorf("resuming lost the summary: %q", resumed.Summary)
	}
}
