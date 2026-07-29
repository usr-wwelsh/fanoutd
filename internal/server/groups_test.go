package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"fanoutd/internal/agent"
	"fanoutd/internal/config"
	"fanoutd/internal/models"
	"fanoutd/internal/store"
)

// The group endpoints are the first ones addressed by something other than a
// task id, so the routing is worth pinning: /api/groups/:id, its actions, and
// the shapes that must not resolve to either.

func groupServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	loop := agent.NewLoop(s, nil, filepath.Join(dir, "output"))
	return New(s, loop, nil, config.Config{}, fstest.MapFS{}), s
}

// seedGroup creates a two-subtask breakdown with the claims that give it a
// shape, which is all the plan endpoint reads.
func seedGroup(t *testing.T, s *store.Store, groupID string) []models.Task {
	t.Helper()
	workspace := "ws-" + groupID
	specs := []struct {
		title  string
		writes []string
		reads  []string
	}{
		{"schema", []string{"schema.json"}, nil},
		{"impl", []string{"board.js"}, []string{"schema.json"}},
	}

	tasks := []models.Task{}
	for _, spec := range specs {
		task, err := s.CreateTaskFrom(store.NewTask{
			Title: spec.title, Goal: spec.title, GroupID: groupID, WorkspaceID: workspace,
		})
		if err != nil {
			t.Fatalf("CreateTaskFrom: %v", err)
		}
		if _, err := s.DeclareWrites(workspace, task.ID, spec.writes); err != nil {
			t.Fatalf("DeclareWrites: %v", err)
		}
		if err := s.DeclareReads(workspace, task.ID, spec.reads); err != nil {
			t.Fatalf("DeclareReads: %v", err)
		}
		tasks = append(tasks, *task)
	}
	return tasks
}

func call(t *testing.T, srv *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	srv.handleGroupRoute(w, httptest.NewRequest(method, path, nil))
	return w
}

func TestGroupPlanServesTheResolvedWaves(t *testing.T) {
	srv, s := groupServer(t)
	tasks := seedGroup(t, s, "grp1")

	for _, path := range []string{"/api/groups/grp1", "/api/groups/grp1/plan"} {
		w := call(t, srv, http.MethodGet, path)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", path, w.Code, w.Body)
		}
		var plan models.GroupPlan
		if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		// The edge is derived from the file partition, not declared: impl reads
		// what schema writes, so it cannot share a wave with it.
		if len(plan.Waves) != 2 || plan.Waves[0][0] != tasks[0].ID {
			t.Errorf("%s: waves = %v, want schema then impl", path, plan.Waves)
		}
		if len(plan.Tasks) != 2 {
			t.Errorf("%s: got %d tasks, want the group's 2", path, len(plan.Tasks))
		}
		if plan.Running {
			t.Errorf("%s: reported running with no schedule attached", path)
		}
	}
}

func TestGroupRoutesRejectBadRequests(t *testing.T) {
	srv, s := groupServer(t)
	seedGroup(t, s, "grp1")

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/groups/nope/plan", http.StatusNotFound},
		{http.MethodGet, "/api/groups/", http.StatusBadRequest},
		{http.MethodGet, "/api/groups/grp1/plan/extra", http.StatusBadRequest},
		{http.MethodGet, "/api/groups/grp1/frobnicate", http.StatusBadRequest},
		{http.MethodPost, "/api/groups/grp1/plan", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/groups/grp1/stop", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/groups/grp1/move", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/api/groups/nope", http.StatusNotFound},
		{http.MethodPost, "/api/groups/grp1/move", http.StatusBadRequest}, // no body
	}
	for _, tc := range cases {
		if w := call(t, srv, tc.method, tc.path); w.Code != tc.want {
			t.Errorf("%s %s: status %d, want %d (%s)", tc.method, tc.path, w.Code, tc.want, w.Body)
		}
	}
}

// Stopping a group that is not running is not an error: the caller wanted it
// stopped, and it is. The response is the plan either way.
func TestStopGroupIsIdempotent(t *testing.T) {
	srv, s := groupServer(t)
	seedGroup(t, s, "grp1")

	w := call(t, srv, http.MethodPost, "/api/groups/grp1/stop")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var plan models.GroupPlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Running {
		t.Error("a stopped group still reports running")
	}
}

// The board renders a breakdown as one card, so a move has to reach every
// subtask. A plan half in To-Do and half in Finished has no card to draw.
func TestMoveGroupFilesEverySubtask(t *testing.T) {
	srv, s := groupServer(t)
	seedGroup(t, s, "grp1")

	w := httptest.NewRecorder()
	srv.handleGroupRoute(w, httptest.NewRequest(http.MethodPost, "/api/groups/grp1/move",
		strings.NewReader(`{"column": "finished"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	tasks, err := s.TasksInGroup("grp1")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.Column != "finished" {
			t.Errorf("%s is in %q, want finished", task.Title, task.Column)
		}
	}

	w = httptest.NewRecorder()
	srv.handleGroupRoute(w, httptest.NewRequest(http.MethodPost, "/api/groups/grp1/move",
		strings.NewReader(`{"column": "nowhere"}`)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("an unknown column got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// Deleting a group is one action because the subtasks share one workspace:
// removing them one at a time leaves the directory behind until the last, which
// is exactly the ordering a caller should not have to know about.
func TestDeleteGroupRemovesEverySubtaskAndTheSharedWorkspace(t *testing.T) {
	srv, s := groupServer(t)
	tasks := seedGroup(t, s, "grp1")

	ws, err := srv.loop.Workspace(tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ws.Root(), 0o755); err != nil {
		t.Fatal(err)
	}

	if w := call(t, srv, http.MethodDelete, "/api/groups/grp1"); w.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	remaining, err := s.TasksInGroup("grp1")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Errorf("%d subtasks survived the group delete", len(remaining))
	}
	if _, err := os.Stat(ws.Root()); !os.IsNotExist(err) {
		t.Errorf("the shared workspace %s is still on disk", ws.Root())
	}
}

func TestBreakdownRejectsAnEmptyIdea(t *testing.T) {
	srv, _ := groupServer(t)
	w := httptest.NewRecorder()
	srv.handleBreakdown(w, httptest.NewRequest(http.MethodPost, "/api/breakdown",
		strings.NewReader(`{"idea": "   "}`)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want %d", w.Code, http.StatusBadRequest)
	}

	// A GET must not reach the model call at all.
	w = httptest.NewRecorder()
	srv.handleBreakdown(w, httptest.NewRequest(http.MethodGet, "/api/breakdown", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}
