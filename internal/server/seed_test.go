package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fanoutd/internal/agent"
	"fanoutd/internal/models"
)

// A seed reaches the workspace before the task can be started, which is the
// whole contract: the agent's first list_files must already show it.
func TestCreateTaskInstallsTheSeed(t *testing.T) {
	srv, _ := groupServer(t)

	body := `{"title": "port a script", "goal": "port it",
	          "seed": [{"path": "spec.md", "content": "the brief"},
	                   {"path": "src/old.py", "content": "print(1)"}]}`
	w := httptest.NewRecorder()
	srv.createTask(w, httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	var task models.Task
	if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
		t.Fatalf("decoding the task: %v", err)
	}
	ws, err := srv.loop.Workspace(task.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	for path, want := range map[string]string{"spec.md": "the brief", "src/old.py": "print(1)"} {
		got, err := os.ReadFile(filepath.Join(ws.Root(), filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

// A seed that cannot be installed is rejected before the row is written, so the
// board does not fill up with tasks the caller did not mean to create.
func TestCreateTaskRejectsABadSeedWithoutCreatingIt(t *testing.T) {
	srv, store := groupServer(t)

	for _, seed := range []string{
		`[{"path": "../escaped.md", "content": "x"}]`,
		fmt.Sprintf(`[{"path": "big.txt", "content": %q}]`, strings.Repeat("x", agent.MaxSeedFileBytes+1)),
		`[{"path": "a.md", "content": "1"}, {"path": "./a.md", "content": "2"}]`,
	} {
		w := httptest.NewRecorder()
		srv.createTask(w, httptest.NewRequest(http.MethodPost, "/api/tasks",
			strings.NewReader(`{"title": "t", "seed": `+seed+`}`)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("seed %.40s: status %d, want %d", seed, w.Code, http.StatusBadRequest)
		}
	}

	tasks, err := store.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("%d tasks were created by rejected seeds", len(tasks))
	}
}

// The same check runs before the breakdown's model call, which is the expensive
// half of that endpoint.
func TestBreakdownRejectsABadSeedBeforeTheModelCall(t *testing.T) {
	srv, _ := groupServer(t)
	w := httptest.NewRecorder()
	srv.handleBreakdown(w, httptest.NewRequest(http.MethodPost, "/api/breakdown",
		strings.NewReader(`{"idea": "build it", "seed": [{"path": "..", "content": "x"}]}`)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want %d", w.Code, http.StatusBadRequest)
	}
}
