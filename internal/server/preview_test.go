package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fanoutd/internal/config"
	"fanoutd/internal/models"
)

// previewTask writes a small site into a task's workspace and returns the task.
func previewTask(t *testing.T) (*Server, *models.Task) {
	t.Helper()
	srv, store := groupServer(t)
	task, err := store.CreateTask("a page", "", "build it", "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	ws, err := srv.loop.Workspace(task.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(ws.Root(), "assets"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	files := map[string]string{
		"index.html":     `<script src="assets/app.js"></script>`,
		"assets/app.js":  "console.log(1)",
		"assets/app.css": "body{}",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(ws.Root(), filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", rel, err)
		}
	}
	return srv, task
}

func get(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	srv.handlePreview(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// The point of serving the workspace rather than one file: a page's relative
// links resolve to the files beside it, which is what /raw cannot do.
func TestPreviewServesAPageAndItsAssets(t *testing.T) {
	srv, task := previewTask(t)

	for path, want := range map[string]string{
		"/preview/" + task.ID + "/assets/app.js":  "console.log(1)",
		"/preview/" + task.ID + "/assets/app.css": "body{}",
		// The directory serves its index, so the workspace root opens the site.
		"/preview/" + task.ID + "/": `<script src="assets/app.js"></script>`,
	} {
		w := get(t, srv, path)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: status %d: %s", path, w.Code, w.Body.String())
			continue
		}
		if got := w.Body.String(); got != want {
			t.Errorf("GET %s = %q, want %q", path, got, want)
		}
	}
}

// Without the trailing slash a relative link would resolve a level up, outside
// the workspace, so the bare id redirects rather than serving the wrong base.
// Naming index.html canonicalises the same way, onto the directory that is its
// real base.
func TestPreviewRedirectsToTheDirectory(t *testing.T) {
	srv, task := previewTask(t)

	for path, want := range map[string]string{
		"/preview/" + task.ID:                 "/preview/" + task.ID + "/",
		"/preview/" + task.ID + "/index.html": "./",
	} {
		w := get(t, srv, path)
		if w.Code != http.StatusMovedPermanently {
			t.Errorf("GET %s: status %d, want %d", path, w.Code, http.StatusMovedPermanently)
			continue
		}
		if got := w.Header().Get("Location"); got != want {
			t.Errorf("GET %s: Location = %q, want %q", path, got, want)
		}
	}
}

func TestPreviewStaysInsideTheWorkspace(t *testing.T) {
	srv, task := previewTask(t)

	for _, path := range []string{
		"/preview/" + task.ID + "/../../fanoutd.db",
		"/preview/" + task.ID + "/assets/../../../etc/passwd",
	} {
		w := get(t, srv, path)
		if w.Code == http.StatusOK {
			t.Errorf("GET %s served %d bytes, want it refused", path, w.Body.Len())
		}
	}
}

func TestPreviewRejectsAnUnknownTask(t *testing.T) {
	srv, _ := previewTask(t)

	if w := get(t, srv, "/preview/nope/index.html"); w.Code != http.StatusNotFound {
		t.Errorf("unknown task: status %d, want %d", w.Code, http.StatusNotFound)
	}
	if w := get(t, srv, "/preview/"); w.Code != http.StatusBadRequest {
		t.Errorf("missing id: status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// Workspace output is the board's contents, so a token gates it exactly as it
// gates the API - being HTML does not make it public.
func TestPreviewNeedsTheToken(t *testing.T) {
	srv, task := previewTask(t)
	srv.cfg = config.Config{Token: "secret"}
	handler := srv.withAuth(http.HandlerFunc(srv.handlePreview))
	path := "/preview/" + task.ID + "/"

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("without a token: status %d, want %d", w.Code, http.StatusUnauthorized)
	}

	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("with a token: status %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "assets/app.js") {
		t.Errorf("body = %q, want the page", w.Body.String())
	}
}
