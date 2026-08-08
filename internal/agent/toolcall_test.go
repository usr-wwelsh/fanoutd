package agent

import (
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

	"fanoutd/internal/models"
	"fanoutd/internal/llm"
	"fanoutd/internal/store"
)

// These drive a whole run against a model that answers with native tool calls,
// which is the path every capable model takes and the one the JSON-protocol
// tests never exercise.

// nativeCall is one call in a scripted turn.
type nativeCall struct {
	id   string
	name string
	args map[string]any
}

// nativeTurn is one model reply: prose, calls, or both.
type nativeTurn struct {
	content string
	calls   []nativeCall
}

// nativeModel replays scripted turns and keeps every request body it was sent,
// so a test can assert on the transcript the loop built rather than only on what
// the run left on disk.
type nativeModel struct {
	mu       sync.Mutex
	turns    []nativeTurn
	calls    int
	requests []chatBody
}

type chatBody struct {
	Messages []struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
		Name       string `json:"name"`
		ToolCalls  []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
}

func (m *nativeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)

	m.mu.Lock()
	var body chatBody
	json.Unmarshal(raw, &body)
	m.requests = append(m.requests, body)

	turn := m.turns[len(m.turns)-1]
	if m.calls < len(m.turns) {
		turn = m.turns[m.calls]
	}
	m.calls++
	m.mu.Unlock()

	delta := map[string]any{"content": turn.content}
	if len(turn.calls) > 0 {
		encoded := make([]any, 0, len(turn.calls))
		for i, c := range turn.calls {
			args, _ := json.Marshal(c.args)
			encoded = append(encoded, map[string]any{
				"index": i, "id": c.id, "type": "function",
				"function": map[string]any{"name": c.name, "arguments": string(args)},
			})
		}
		delta["tool_calls"] = encoded
	}

	frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
}

func nativeLoop(t *testing.T, turns ...nativeTurn) (*Loop, *store.Store, *nativeModel) {
	t.Helper()
	model := &nativeModel{turns: turns}
	srv := httptest.NewServer(model)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	s, err := store.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	client := llm.NewClient(llm.Preset{Name: "test"}, "test-key", "test-model", srv.URL)
	l := NewLoop(s, client, filepath.Join(dir, "output"))
	stopEverything(t, l)
	return l, s, model
}

func write(id, path, content string) nativeCall {
	return nativeCall{id: id, name: "write_file", args: map[string]any{"path": path, "content": content}}
}

// The bug this exists for: a turn making three calls had two of them dropped on
// the floor, unexecuted and unanswered, while the model went on believing all
// three files were written.
func TestEveryToolCallInATurnRuns(t *testing.T) {
	l, s, _ := nativeLoop(t,
		nativeTurn{content: "Writing all three.", calls: []nativeCall{
			write("c1", "a.md", "alpha"),
			write("c2", "b.md", "beta"),
			write("c3", "c.md", "gamma"),
		}},
		nativeTurn{calls: []nativeCall{{id: "c4", name: "finish", args: map[string]any{"summary": "wrote three files"}}}},
	)

	task := runToEnd(t, l, s, "write three files")
	if task.Status != models.StatusDone {
		t.Fatalf("status = %q, want done (%s)", task.Status, task.Error)
	}

	ws, err := l.Workspace(task.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	for path, want := range map[string]string{"a.md": "alpha", "b.md": "beta", "c.md": "gamma"} {
		got, err := os.ReadFile(filepath.Join(ws.Root(), path))
		if err != nil {
			t.Fatalf("%s was never written: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}

	steps, err := s.ListTraceSteps(task.ID)
	if err != nil {
		t.Fatalf("ListTraceSteps: %v", err)
	}
	if len(steps) == 0 || len(steps[0].Calls) != 3 {
		t.Fatalf("trace recorded %d calls for the first step, want 3", len(steps[0].Calls))
	}
	for i, c := range steps[0].Calls {
		if c.ID == "" || c.Result == "" {
			t.Errorf("call %d recorded without an id or a result: %+v", i, c)
		}
	}
}

// The transcript a run builds decides how well the model works in it. A tool
// call has to go back as a tool call — assistant.tool_calls answered by a "tool"
// message on the same id — or the model cannot see the arguments it sent.
func TestToolCallsAreReplayedNatively(t *testing.T) {
	l, s, model := nativeLoop(t,
		nativeTurn{content: "Writing the page.", calls: []nativeCall{write("c1", "index.html", "<html>hello</html>")}},
		nativeTurn{calls: []nativeCall{{id: "c2", name: "finish", args: map[string]any{"summary": "done"}}}},
	)
	runToEnd(t, l, s, "write index.html")

	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.requests) < 2 {
		t.Fatalf("the model was called %d times, want at least 2", len(model.requests))
	}

	second := model.requests[1]
	var assistant, tool int
	for _, m := range second.Messages {
		switch m.Role {
		case "assistant":
			assistant++
			if len(m.ToolCalls) != 1 {
				t.Fatalf("assistant turn replayed with %d tool calls, want 1", len(m.ToolCalls))
			}
			if m.ToolCalls[0].ID != "c1" || m.ToolCalls[0].Function.Name != "write_file" {
				t.Errorf("assistant turn replayed the wrong call: %+v", m.ToolCalls[0])
			}
			// The whole point: the model can see what it wrote.
			if !jsonHas(m.ToolCalls[0].Function.Arguments, "content", "<html>hello</html>") {
				t.Errorf("arguments were not replayed: %q", m.ToolCalls[0].Function.Arguments)
			}
		case "tool":
			tool++
			if m.ToolCallID != "c1" {
				t.Errorf("tool result keyed on %q, want c1 — an unmatched id is dropped by the provider", m.ToolCallID)
			}
			if m.Name != "write_file" {
				t.Errorf("tool result named %q, want write_file", m.Name)
			}
		case "user":
			if m.Content == "Continue. What is your next action?" {
				t.Error("a native exchange still got the prose nudge, which is not the shape models are trained on")
			}
		}
	}
	if assistant != 1 || tool != 1 {
		t.Fatalf("replayed %d assistant and %d tool messages, want 1 and 1", assistant, tool)
	}
}

// A step whose calls carry no ids — the JSON fallback protocol, and every row
// written before calls were recorded — must still replay as prose. A "tool"
// message answering a call the assistant turn never made is rejected outright by
// some providers.
func TestStepsWithoutIDsReplayAsProse(t *testing.T) {
	native := models.TraceStep{
		Response: "writing",
		Calls: []models.ToolExchange{
			{ID: "c1", Name: "write_file", Arguments: `{"path":"a.md"}`, Result: "wrote a.md"},
		},
	}
	if !replayable(native) {
		t.Error("a step with ids on every call should replay natively")
	}

	native.Calls = append(native.Calls, models.ToolExchange{Name: "read_file", Result: "a"})
	if replayable(native) {
		t.Error("one call missing an id must force the whole step onto the prose path")
	}

	if replayable(models.TraceStep{ToolName: "write_file", ToolResult: "wrote a.md"}) {
		t.Error("a legacy row has no calls to replay")
	}
}

// finish arriving in the same turn as the work it signs off on must not cost
// that work.
func TestFinishAlongsideAWriteKeepsTheFile(t *testing.T) {
	l, s, _ := nativeLoop(t,
		nativeTurn{content: "Done.", calls: []nativeCall{
			write("c1", "report.md", "the report"),
			{id: "c2", name: "finish", args: map[string]any{"summary": "wrote report.md"}},
		}},
	)

	task := runToEnd(t, l, s, "write report.md")
	if task.Status != models.StatusDone {
		t.Fatalf("status = %q, want done (%s)", task.Status, task.Error)
	}
	if task.Summary != "wrote report.md" {
		t.Errorf("summary = %q, want the model's own", task.Summary)
	}

	ws, _ := l.Workspace(task.ID)
	got, err := os.ReadFile(filepath.Join(ws.Root(), "report.md"))
	if err != nil {
		t.Fatalf("the write beside finish was discarded: %v", err)
	}
	if string(got) != "the report" {
		t.Errorf("report.md = %q", got)
	}
}

// A large write must not carry the whole file back into every later request,
// but the call still has to be there for the model to see it made it.
func TestOversizeArgumentsAreClippedToValidJSON(t *testing.T) {
	big := make([]byte, toolCallBudget+100)
	for i := range big {
		big[i] = 'x'
	}
	args := `{"path":"big.txt","content":"` + string(big) + `"}`

	got := replayArgs(args)
	if len(got) > 1000 {
		t.Fatalf("clipped arguments are still %d bytes", len(got))
	}
	var probe map[string]string
	if err := json.Unmarshal([]byte(got), &probe); err != nil {
		t.Fatalf("clipped arguments are not valid JSON: %v (%q)", err, got)
	}

	small := `{"path":"a.md","content":"hi"}`
	if replayArgs(small) != small {
		t.Error("arguments within the budget must be replayed verbatim")
	}
}

// The trace is what the board and the CLI render, and they read one name and one
// result. A turn that made several calls has to say so there too.
func TestBatchIsSummarizedForDisplay(t *testing.T) {
	name, result := summarizeExchanges([]models.ToolExchange{
		{Name: "write_file", Result: "wrote a.md (1 bytes)"},
		{Name: "write_file", Result: "wrote b.md (1 bytes)"},
	})
	if name != "write_file +1" {
		t.Errorf("name = %q, want the batch size", name)
	}
	for _, want := range []string{"a.md", "b.md"} {
		if !strings.Contains(result, want) {
			t.Errorf("result %q dropped %s", result, want)
		}
	}

	if name, result := summarizeExchanges(nil); name != "" || result != "" {
		t.Errorf("empty batch = (%q, %q)", name, result)
	}
}

// jsonHas reports whether an encoded object carries key with the given value.
func jsonHas(encoded, key, value string) bool {
	var probe map[string]any
	if err := json.Unmarshal([]byte(encoded), &probe); err != nil {
		return false
	}
	got, ok := probe[key].(string)
	return ok && got == value
}
