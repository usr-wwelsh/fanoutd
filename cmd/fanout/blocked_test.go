package main

import (
	"strings"
	"testing"

	"fanoutd/internal/models"
)

// boardWithBlocked is one of each thing that ends a run, plus the two that must
// not be listed: a live run and a task somebody already filed.
func boardWithBlocked() *board {
	b := newBoard()
	b.tasks = []models.Task{
		{ID: "31fcf150-0000-0000-0000-000000000001", Title: "Game entry point and UI",
			Column: "todo", Status: models.StatusStopped,
			Summary: `Stopped after 49 steps without calling finish (agent repeated the same action 5 times without making progress: "Let me verify the final state of ` + "`src/game.js`" + `."). The workspace holds 2 files: index.html, src/game.js.`},
		{ID: "d19862c0-0000-0000-0000-000000000002", Title: "spreadsheet engine",
			Column: "todo", Status: models.StatusError,
			Error: "agent repeated the same read_file call 3 times without making progress"},
		{ID: "acebc620-0000-0000-0000-000000000003", Title: "restarted mid-run",
			Column: "todo", Status: models.StatusStopped,
			Error: "interrupted by a server restart"},
		{ID: "e6a29400-0000-0000-0000-000000000004", Title: "still going",
			Column: "todo", Status: models.StatusRunning},
		{ID: "c762903a-0000-0000-0000-000000000005", Title: "already dealt with",
			Column: "finished", Status: models.StatusError, Error: "gave up"},
	}
	return b
}

func TestBlockedListsWhyWithoutTheTrace(t *testing.T) {
	b := boardWithBlocked()
	// A trace exists; the whole point is that blocked never asks for it.
	b.trace[b.tasks[0].ID] = []models.TraceStep{{StepNumber: 1, Response: strings.Repeat("z", 4000)}}

	code, out := runCLI(t, b, "blocked")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{
		"31fcf15", "repeated the same action 5 times",
		"d19862c", "repeated the same read_file call 3 times",
		"acebc62", "interrupted by a server restart",
		"3 blocked tasks",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("blocked output missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"still going", "already dealt with", "zzzz", "Let me verify"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("blocked output should not contain %q:\n%s", unwanted, out)
		}
	}
}

func TestBlockedAllIncludesFiledTasks(t *testing.T) {
	code, out := runCLI(t, boardWithBlocked(), "blocked", "--all")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "already dealt with") {
		t.Errorf("--all should reach the finished column:\n%s", out)
	}
	if strings.Contains(out, "still going") {
		t.Errorf("--all is not a reason to list a running task:\n%s", out)
	}
}

func TestBlockedIsQuietWhenNothingIsStuck(t *testing.T) {
	code, out := runCLI(t, boardWithTask(models.StatusRunning), "blocked")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if strings.TrimSpace(out) != "nothing blocked" {
		t.Errorf("want a one-line all-clear, got:\n%s", out)
	}
}

func TestBlockedResumeStartsEveryListedTask(t *testing.T) {
	b := boardWithBlocked()
	code, out := runCLI(t, b, "blocked", "--resume")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, id := range []string{b.tasks[0].ID, b.tasks[1].ID, b.tasks[2].ID} {
		if got := b.task(id).Status; got != models.StatusRunning {
			t.Errorf("task %s is %s after --resume, want running", shortID(id), got)
		}
	}
	if got := b.task(b.tasks[4].ID).Status; got != models.StatusError {
		t.Errorf("--resume touched a task outside the default scope: %s", got)
	}
	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; n != 3 {
		t.Errorf("want one line per resumed task, got %d:\n%s", n, out)
	}
}

func TestBlockedNamesTheGroupOfASubtask(t *testing.T) {
	b := boardWithBlocked()
	b.tasks[0].GroupID = "9f232c7a-0000-0000-0000-0000000000aa"

	_, out := runCLI(t, b, "blocked")
	if !strings.Contains(out, "group 9f232c7") {
		t.Errorf("a blocked subtask should point at its group:\n%s", out)
	}
}

func TestBlockReasonFallsBackToTheStatus(t *testing.T) {
	got := blockReason(models.Task{Status: models.StatusStopped})
	if got != "stopped, no reason recorded" {
		t.Errorf("blockReason with nothing recorded = %q", got)
	}
}
