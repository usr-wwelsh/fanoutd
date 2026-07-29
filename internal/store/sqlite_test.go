package store

import (
	"path/filepath"
	"testing"

	"fanoutd/internal/models"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestReclaimRunningTasks(t *testing.T) {
	s := testStore(t)

	running, err := s.CreateTask("running", "", "goal", "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	done, err := s.CreateTask("done", "", "goal", "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if err := s.SetTaskStatus(running.ID, models.StatusRunning, ""); err != nil {
		t.Fatalf("SetTaskStatus: %v", err)
	}
	if err := s.SetTaskStatus(done.ID, models.StatusDone, ""); err != nil {
		t.Fatalf("SetTaskStatus: %v", err)
	}

	n, err := s.ReclaimRunningTasks()
	if err != nil {
		t.Fatalf("ReclaimRunningTasks: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d tasks, want 1", n)
	}

	got, err := s.GetTask(running.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != models.StatusStopped {
		t.Errorf("status = %q, want %q", got.Status, models.StatusStopped)
	}
	if got.Error == "" {
		t.Error("want a reason on the reclaimed task, got none")
	}

	// Tasks that were not running are left alone.
	other, err := s.GetTask(done.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if other.Status != models.StatusDone {
		t.Errorf("untouched task status = %q, want %q", other.Status, models.StatusDone)
	}

	// A second pass on a clean database is a no-op, so a restart that stopped
	// cleanly does not annotate anything.
	if n, err = s.ReclaimRunningTasks(); err != nil || n != 0 {
		t.Fatalf("second pass reclaimed %d (err %v), want 0", n, err)
	}
}
