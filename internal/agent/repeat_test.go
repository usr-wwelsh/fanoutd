package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fanoutd/internal/models"
	"fanoutd/internal/llm"
	"fanoutd/internal/store"
)

// scriptedModel replays a fixed sequence of replies, one per step, holding on
// the last once the script runs out. The repeat guard is a property of the
// sequence of calls rather than of any one of them, so it can only be tested by
// driving a whole run.
type scriptedModel struct {
	mu      sync.Mutex
	replies []string
	calls   int
}

func (m *scriptedModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)

	m.mu.Lock()
	reply := m.replies[len(m.replies)-1]
	if m.calls < len(m.replies) {
		reply = m.replies[m.calls]
	}
	m.calls++
	m.mu.Unlock()

	frame, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": reply}}},
	})
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
}

// step renders one reply in the JSON protocol, which is the path a model without
// native tool calls takes and the cheapest one to write by hand.
func step(action string, tool map[string]any) string {
	body := map[string]any{"goal_met": false, "next_action": action}
	if tool != nil {
		body["tool"] = tool
	}
	out, _ := json.Marshal(body)
	return string(out)
}

func finish(summary string) string {
	out, _ := json.Marshal(map[string]any{"goal_met": true, "summary": summary})
	return string(out)
}

func scriptedLoop(t *testing.T, replies ...string) (*Loop, *store.Store) {
	t.Helper()
	srv := httptest.NewServer(&scriptedModel{replies: replies})
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	s, err := store.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	client := llm.NewClient(llm.Preset{Name: "test"}, "test-key", "test-model", srv.URL)
	l := NewLoop(s, client, filepath.Join(dir, "output"))
	stopEverything(t, l)
	return l, s
}

// runToEnd starts a task and waits for the loop to record a final status.
func runToEnd(t *testing.T, l *Loop, s *store.Store, goal string) *models.Task {
	t.Helper()
	task, err := s.CreateTaskFrom(store.NewTask{Title: "t", Goal: goal})
	if err != nil {
		t.Fatalf("CreateTaskFrom: %v", err)
	}
	if err := l.Start(task.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := 0; i < 400 && l.IsRunning(task.ID); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if l.IsRunning(task.ID) {
		l.Stop(task.ID)
		t.Fatal("the run never ended")
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return got
}

func traceMentions(t *testing.T, s *store.Store, taskID, want string) bool {
	t.Helper()
	steps, err := s.ListTraceSteps(taskID)
	if err != nil {
		t.Fatalf("ListTraceSteps: %v", err)
	}
	for _, ts := range steps {
		if bytes.Contains([]byte(ts.ToolResult), []byte(want)) {
			return true
		}
	}
	return false
}

// The regression this whole change exists for. Verifying an edit by reading the
// file back is the loop working: the file is different each time, and so is the
// answer. Counting those reads as repeats aborted runs that were converging —
// and because the abort concedes rather than fails, the agent was restarted
// straight back into it and conceded again on its first step.
func TestReadingBackAnEditIsNotARepeat(t *testing.T) {
	const verify = "Let me verify the file was written correctly."
	read := map[string]any{"name": "read_file", "path": "a.md"}

	l, s := scriptedLoop(t,
		step("Write the file.", map[string]any{"name": "write_file", "path": "a.md", "content": "one"}),
		step(verify, read),
		step("Fix it.", map[string]any{"name": "edit_file", "path": "a.md", "old": "one", "new": "two"}),
		step(verify, read),
		step("Fix it again.", map[string]any{"name": "edit_file", "path": "a.md", "old": "two", "new": "three"}),
		step(verify, read),
		finish("wrote a.md"),
	)

	task := runToEnd(t, l, s, "write a.md")
	if traceMentions(t, s, task.ID, "aborted") {
		t.Error("a read that followed an edit was counted as a repeat")
	}
	if task.Status != models.StatusDone {
		t.Errorf("status = %q, want done (%s)", task.Status, task.Summary)
	}
}

// The other half of the same rule: the guard still has to fire. Three identical
// reads of a file nothing has touched is the loop it was written for.
func TestRereadingAnUnchangedFileStillAborts(t *testing.T) {
	const verify = "Let me verify the file was written correctly."
	read := map[string]any{"name": "read_file", "path": "a.md"}

	l, s := scriptedLoop(t,
		step("Write the file.", map[string]any{"name": "write_file", "path": "a.md", "content": "one"}),
		step(verify, read),
	)

	task := runToEnd(t, l, s, "write a.md")
	if !traceMentions(t, s, task.ID, "repeated") {
		t.Error("an unchanging read loop ran to the step limit instead of being caught")
	}
	// It wrote a file before it stalled, so the run concedes to done rather than
	// erroring — the deliverable is real even though the agent never signed off.
	if task.Status != models.StatusDone {
		t.Errorf("status = %q, want done", task.Status)
	}
}

// Writing the same bytes to the same path is a repeat however much else has
// moved, so a mutating call is judged on its arguments alone.
func TestRewritingTheSameContentIsARepeat(t *testing.T) {
	l, s := scriptedLoop(t,
		step("Write it.", map[string]any{"name": "write_file", "path": "a.md", "content": "one"}),
	)

	task := runToEnd(t, l, s, "write a.md")
	if !traceMentions(t, s, task.ID, "repeated") {
		t.Error("an identical write repeated forever was not caught")
	}
}
