package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"fanoutd/internal/llm"
	"fanoutd/internal/models"
	"fanoutd/internal/store"
)

// namedModel answers nothing useful; it exists to say which client was asked.
type namedModel struct {
	name   string
	asked  chan string
	result *llm.Result
}

func (m *namedModel) Chat(context.Context, []llm.MsgBlock, llm.ChatOptions) (*llm.Result, error) {
	select {
	case m.asked <- m.name:
	default:
	}
	return m.result, nil
}

// Changing the provider in the settings has to reach the runs, or the operator
// switches endpoints and every task keeps billing the old key until the process
// is restarted — which is the restart the settings page exists to avoid.
func TestSwappingTheClientChangesWhatTheNextRunCalls(t *testing.T) {
	dir := t.TempDir()
	s, err := store.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	asked := make(chan string, 4)
	finish := &llm.Result{Content: `{"goal_met": true, "summary": "done"}`}

	l := NewLoop(s, &namedModel{name: "first", asked: asked, result: finish}, filepath.Join(dir, "output"))
	stopEverything(t, l)

	l.SetClient(&namedModel{name: "second", asked: asked, result: finish})

	task, err := s.CreateTaskFrom(store.NewTask{Title: "t", Goal: "g"})
	if err != nil {
		t.Fatalf("CreateTaskFrom: %v", err)
	}
	if err := l.Start(task.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case got := <-asked:
		if got != "second" {
			t.Errorf("the run called the %s client, want the one just set", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no client was called at all")
	}
	if got := waitDone(t, s, task.ID); got != models.StatusDone {
		t.Errorf("status = %q, want %q", got, models.StatusDone)
	}
}

// waitDone polls until the run records a final status, since Start returns as
// soon as the goroutine is claimed.
func waitDone(t *testing.T, s *store.Store, id string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := s.GetTask(id)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if task != nil && task.Status != models.StatusRunning {
			return task.Status
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the run never settled")
	return ""
}

// Clearing a limit on the settings page has to put the default back. Ignoring
// the empty value would leave the last number the operator typed in force with
// nothing on screen saying so, and only a restart would shift it.
func TestClearingALimitRestoresTheDefault(t *testing.T) {
	l := NewLoop(nil, nil, t.TempDir())

	l.SetMaxSteps(7)
	l.SetMaxParallel(9)
	if l.stepLimit() != 7 || l.parallelLimit() != 9 {
		t.Fatalf("limits = %d/%d, want 7/9", l.stepLimit(), l.parallelLimit())
	}

	l.SetMaxSteps(0)
	l.SetMaxParallel(0)
	if got := l.stepLimit(); got != defaultMaxSteps {
		t.Errorf("step limit = %d after clearing, want the default %d", got, defaultMaxSteps)
	}
	if got := l.parallelLimit(); got != defaultMaxParallel {
		t.Errorf("parallel limit = %d after clearing, want the default %d", got, defaultMaxParallel)
	}
}
