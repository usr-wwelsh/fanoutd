package main

import (
	"strings"
	"testing"

	"fanoutd/internal/models"
)

const testGroupID = "g1110000-0000-0000-0000-000000000001"

func subtask(id, title, status string) models.Task {
	return models.Task{ID: id, Title: title, Status: status, Column: "todo", GroupID: testGroupID}
}

// boardWithGroup wires a three-subtask breakdown in a two-wave shape.
func boardWithGroup(status string) (*board, *models.GroupPlan) {
	tasks := []models.Task{
		subtask("aaaa0001-0000-0000-0000-000000000001", "schema", status),
		subtask("aaaa0002-0000-0000-0000-000000000002", "impl", status),
		subtask("aaaa0003-0000-0000-0000-000000000003", "docs", status),
	}
	plan := &models.GroupPlan{
		GroupID: testGroupID,
		Waves:   [][]string{{tasks[0].ID}, {tasks[1].ID, tasks[2].ID}},
		Deps:    map[string][]string{tasks[1].ID: {tasks[0].ID}, tasks[2].ID: {tasks[0].ID}},
		Tasks:   tasks,
	}
	b := newBoard()
	b.tasks = append(b.tasks, tasks...)
	b.groups[testGroupID] = plan
	return b, plan
}

func TestBreakdownPrintsTheWavePlan(t *testing.T) {
	b, plan := boardWithGroup(models.StatusIdle)
	b.tasks = nil // the breakdown response is what puts them on the board
	b.breakdown = &models.BreakdownResult{GroupID: testGroupID, Tasks: plan.Tasks, Plan: plan}

	code, out := runCLI(t, b, "breakdown", "build a board")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{"3 subtasks in 2 waves", "wave 1", "schema", "wave 2", "impl", "docs"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	// Created but not started: say how to start it rather than leaving the
	// group sitting there.
	if !strings.Contains(out, "--start") {
		t.Errorf("an unstarted group should say how to run it:\n%s", out)
	}
}

// The fallback is the expected outcome for an idea that does not divide, so it
// prints as a note and exits zero rather than as a failure.
func TestBreakdownReportsTheFallback(t *testing.T) {
	b := newBoard()
	b.breakdown = &models.BreakdownResult{
		Fallback: "could not split this into parallel subtasks, so it was created as one task: two subtasks cannot write the same file: board.js",
		Tasks:    []models.Task{{ID: "ffff0001-0000-0000-0000-000000000001", Title: "build a board"}},
	}

	code, out := runCLI(t, b, "breakdown", "build a board")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "board.js") {
		t.Errorf("the reason for the fallback should be printed:\n%s", out)
	}
	if !strings.Contains(out, "ffff000") {
		t.Errorf("the single task should be named:\n%s", out)
	}
}

func TestBreakdownRequiresAnIdea(t *testing.T) {
	if code, _ := runCLI(t, newBoard(), "breakdown"); code != exitFailure {
		t.Errorf("exit %d, want %d", code, exitFailure)
	}
}

func TestPlanResolvesAGroupPrefix(t *testing.T) {
	b, _ := boardWithGroup(models.StatusDone)
	code, out := runCLI(t, b, "plan", "g111")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "3 subtasks in 2 waves") {
		t.Errorf("got:\n%s", out)
	}
}

func TestPlanWatchFollowsAGroupToCompletion(t *testing.T) {
	b, plan := boardWithGroup(models.StatusRunning)
	plan.Running = true

	done := *plan
	done.Running = false
	done.Tasks = []models.Task{
		subtask(plan.Tasks[0].ID, "schema", models.StatusDone),
		subtask(plan.Tasks[1].ID, "impl", models.StatusDone),
		subtask(plan.Tasks[2].ID, "docs", models.StatusDone),
	}
	done.Tasks[0].Summary = "wrote schema.json"
	b.groupPlans[testGroupID] = []*models.GroupPlan{plan, &done}

	code, out := runCLI(t, b, "plan", "g111", "--watch", "--interval", "1ms")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "3 subtasks finished") {
		t.Errorf("want a completion line:\n%s", out)
	}
	if !strings.Contains(out, "wrote schema.json") {
		t.Errorf("want each subtask's outcome as it settles:\n%s", out)
	}
}

// A failed subtask fails the command, which is what makes a group composable
// with a shell script the same way a single task is.
func TestPlanWatchExitsNonZeroOnAFailedSubtask(t *testing.T) {
	b, plan := boardWithGroup(models.StatusRunning)
	plan.Running = true

	ended := *plan
	ended.Running = false
	ended.Tasks = []models.Task{
		subtask(plan.Tasks[0].ID, "schema", models.StatusDone),
		subtask(plan.Tasks[1].ID, "impl", models.StatusError),
		subtask(plan.Tasks[2].ID, "docs", models.StatusError),
	}
	ended.Tasks[1].Error = "model call failed at step 3"
	ended.Tasks[2].Error = "skipped: depends on subtask aaaa000, which did not finish"
	b.groupPlans[testGroupID] = []*models.GroupPlan{plan, &ended}

	code, out := runCLI(t, b, "plan", "g111", "--watch", "--interval", "1ms")
	if code != exitTaskError {
		t.Fatalf("exit %d, want %d\n%s", code, exitTaskError, out)
	}
	if !strings.Contains(out, "2 subtasks of 3 failed") {
		t.Errorf("want the failure count:\n%s", out)
	}
	if !strings.Contains(out, "skipped") {
		t.Errorf("a cascaded skip is the reason a subtask never ran; print it:\n%s", out)
	}
}

// A group nobody started never settles on its own. Watching it must return
// rather than poll forever.
func TestPlanWatchDoesNotHangOnAnUnstartedGroup(t *testing.T) {
	b, _ := boardWithGroup(models.StatusIdle)
	code, out := runCLI(t, b, "plan", "g111", "--watch", "--interval", "1ms")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "not running") {
		t.Errorf("want an explanation rather than a poll loop:\n%s", out)
	}
}

// One verb stops either kind of thing: a group id belongs to no task, so it
// falls through to the group endpoints without ambiguity.
func TestStopFallsThroughToAGroup(t *testing.T) {
	b, plan := boardWithGroup(models.StatusRunning)
	plan.Running = true

	code, out := runCLI(t, b, "stop", "g111")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if len(b.stopped) != 1 || b.stopped[0] != testGroupID {
		t.Errorf("stopped %v, want the group", b.stopped)
	}
	if !strings.Contains(out, "3 subtasks") {
		t.Errorf("got:\n%s", out)
	}
}

func TestStopStillReportsAnUnknownId(t *testing.T) {
	b, _ := boardWithGroup(models.StatusIdle)
	if code, _ := runCLI(t, b, "stop", "zzzz"); code != exitFailure {
		t.Errorf("exit %d, want %d", code, exitFailure)
	}
}
