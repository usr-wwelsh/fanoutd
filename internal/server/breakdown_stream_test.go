package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"fanoutd/internal/agent"
	"fanoutd/internal/config"
	"fanoutd/internal/llm"
	"fanoutd/internal/models"
	"fanoutd/internal/store"
)

// The streamed breakdown is what keeps the webui's modal honest while the
// planner works, so the wire shape is pinned here: one JSON object per line,
// stages announced before their work, progress in between, result last. A
// client that does not ask for it still gets the plain single-document reply.

// twoTaskPlan is a partition that survives validation: distinct writers, no
// reads, so no contract is demanded.
const twoTaskPlan = `{"subtasks": [
  {"title": "schema", "goal": "write the schema", "writes": ["schema.json"], "criteria": ["it parses as JSON"]},
  {"title": "impl",   "goal": "write the board", "writes": ["board.js"],   "criteria": ["it mounts"]}
]}`

func streamServer(t *testing.T) *Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		frame, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": twoTaskPlan}}},
		})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	s, err := store.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	client := llm.NewClient(llm.Preset{Name: "test"}, "k", "test-model", srv.URL)
	loop := agent.NewLoop(s, client, filepath.Join(dir, "output"))
	return New(s, loop, nil, config.Config{}, fstest.MapFS{})
}

func postBreakdown(t *testing.T, srv *Server, accept, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/breakdown", strings.NewReader(body))
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	srv.handleBreakdown(w, req)
	return w
}

func TestBreakdownStreamsEventsBeforeTheResult(t *testing.T) {
	srv := streamServer(t)

	w := postBreakdown(t, srv, "application/x-ndjson", `{"idea":"build a board"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/x-ndjson" {
		t.Errorf("content type = %q, want application/x-ndjson", got)
	}

	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	var phases []string
	sawProgress := false
	var result *models.BreakdownResult
	for i, line := range lines {
		var e models.BreakdownEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d (%q): %v", i+1, line, err)
		}
		switch e.Kind {
		case models.BreakdownKindPhase:
			phases = append(phases, e.Phase)
		case models.BreakdownKindProgress:
			sawProgress = true
			if e.Chars == 0 || e.Tail == "" {
				t.Errorf("progress event carries nothing: %s", line)
			}
		case models.BreakdownKindResult:
			result = e.Result
		default:
			t.Fatalf("unknown event kind on line %d: %s", i+1, line)
		}
	}
	wantPhases := []string{models.PhasePlanning, models.PhaseBuilding}
	if len(phases) != len(wantPhases) {
		t.Fatalf("phases = %v, want exactly %v", phases, wantPhases)
	}
	for i := range wantPhases {
		if phases[i] != wantPhases[i] {
			t.Fatalf("phases = %v, want %v", phases, wantPhases)
		}
	}
	if !sawProgress {
		t.Error("the stream carried no progress events")
	}
	if result == nil {
		t.Fatal("the stream ended without a result")
	}
	if result.GroupID == "" || len(result.Tasks) != 2 {
		t.Errorf("result = %+v, want a group of two subtasks", result)
	}
}

// The CLI decodes one JSON document from this endpoint; streaming must stay
// strictly opt-in or every non-browser caller breaks.
func TestBreakdownWithoutStreamingStillAnswersOneDocument(t *testing.T) {
	srv := streamServer(t)

	w := postBreakdown(t, srv, "", `{"idea":"build a board"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("content type = %q, want application/json", got)
	}
	var result models.BreakdownResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("body was not a single breakdown document: %v\n%s", err, w.Body.String())
	}
	if result.GroupID == "" {
		t.Errorf("result = %+v, want a group", result)
	}
}

// Rejections that happen before the model call are ordinary HTTP errors even
// for a streaming client: nothing has been written yet, so the status line can
// still carry them.
func TestBreakdownStreamRefusesABadIdeaBeforeOpening(t *testing.T) {
	srv := streamServer(t)

	w := postBreakdown(t, srv, "application/x-ndjson", `{"idea":"  "}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want a plain 400 before any events", w.Code)
	}
	if strings.Contains(w.Header().Get("Content-Type"), "x-ndjson") {
		t.Errorf("content type = %q, want a normal error response", w.Header().Get("Content-Type"))
	}
}
