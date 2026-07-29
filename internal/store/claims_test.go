package store

import "testing"

func TestClaimWriteIsExclusive(t *testing.T) {
	s := testStore(t)

	owner, err := s.ClaimWrite("ws1", "board.js", "taskA")
	if err != nil {
		t.Fatalf("ClaimWrite: %v", err)
	}
	if owner != "" {
		t.Fatalf("first claim reported owner %q, want it to succeed", owner)
	}

	// The second task is refused and told who holds the path.
	owner, err = s.ClaimWrite("ws1", "board.js", "taskB")
	if err != nil {
		t.Fatalf("ClaimWrite: %v", err)
	}
	if owner != "taskA" {
		t.Fatalf("second claim reported owner %q, want taskA", owner)
	}

	// Re-claiming a path you already hold is not a conflict; the agent writes
	// the same file many times over a run.
	if owner, err = s.ClaimWrite("ws1", "board.js", "taskA"); err != nil || owner != "" {
		t.Fatalf("re-claim reported owner %q (err %v), want success", owner, err)
	}

	// Exclusivity is per workspace, not global.
	if owner, err = s.ClaimWrite("ws2", "board.js", "taskB"); err != nil || owner != "" {
		t.Fatalf("claim in another workspace reported owner %q (err %v), want success", owner, err)
	}
}

func TestDeclareWritesReportsConflicts(t *testing.T) {
	s := testStore(t)

	if _, err := s.ClaimWrite("ws1", "shared.md", "taskA"); err != nil {
		t.Fatalf("ClaimWrite: %v", err)
	}

	conflicts, err := s.DeclareWrites("ws1", "taskB", []string{"own.md", "shared.md", "also-own.md"})
	if err != nil {
		t.Fatalf("DeclareWrites: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1: %+v", len(conflicts), conflicts)
	}
	if conflicts[0].Path != "shared.md" || conflicts[0].Owner != "taskA" {
		t.Errorf("conflict = %+v, want shared.md owned by taskA", conflicts[0])
	}

	// The uncontested paths still landed, so a caller that accepts the plan does
	// not have to declare twice.
	owned, err := s.OwnedPaths("ws1", "taskB")
	if err != nil {
		t.Fatalf("OwnedPaths: %v", err)
	}
	if len(owned) != 2 || owned[0] != "also-own.md" || owned[1] != "own.md" {
		t.Errorf("owned = %v, want [also-own.md own.md]", owned)
	}
}

func TestDeclareReadsAreShared(t *testing.T) {
	s := testStore(t)

	if err := s.DeclareReads("ws1", "taskA", []string{"schema.json"}); err != nil {
		t.Fatalf("DeclareReads: %v", err)
	}
	if err := s.DeclareReads("ws1", "taskB", []string{"schema.json"}); err != nil {
		t.Fatalf("DeclareReads: %v", err)
	}

	for _, id := range []string{"taskA", "taskB"} {
		reads, err := s.ReadPaths("ws1", id)
		if err != nil {
			t.Fatalf("ReadPaths(%s): %v", id, err)
		}
		if len(reads) != 1 || reads[0] != "schema.json" {
			t.Errorf("%s reads = %v, want [schema.json]", id, reads)
		}
	}
}

func TestReleaseClaimsFreesPaths(t *testing.T) {
	s := testStore(t)

	if _, err := s.ClaimWrite("ws1", "board.js", "taskA"); err != nil {
		t.Fatalf("ClaimWrite: %v", err)
	}
	if err := s.DeclareReads("ws1", "taskA", []string{"schema.json"}); err != nil {
		t.Fatalf("DeclareReads: %v", err)
	}
	if err := s.ReleaseClaims("taskA"); err != nil {
		t.Fatalf("ReleaseClaims: %v", err)
	}

	owner, err := s.ClaimWrite("ws1", "board.js", "taskB")
	if err != nil {
		t.Fatalf("ClaimWrite: %v", err)
	}
	if owner != "" {
		t.Fatalf("path still owned by %q after release", owner)
	}
	if reads, err := s.ReadPaths("ws1", "taskA"); err != nil || len(reads) != 0 {
		t.Errorf("reads = %v (err %v), want none after release", reads, err)
	}
}

func TestDeleteTaskReleasesClaims(t *testing.T) {
	s := testStore(t)

	task, err := s.CreateTask("subtask", "", "goal", "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := s.ClaimWrite(task.WorkspaceID, "board.js", task.ID); err != nil {
		t.Fatalf("ClaimWrite: %v", err)
	}
	if err := s.DeleteTask(task.ID); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	// A deleted subtask must not keep a path reserved against its siblings,
	// which share the workspace and outlive it.
	owner, err := s.WriteOwner(task.WorkspaceID, "board.js")
	if err != nil {
		t.Fatalf("WriteOwner: %v", err)
	}
	if owner != "" {
		t.Errorf("owner = %q after delete, want none", owner)
	}
}

func TestGroupIDRoundTrips(t *testing.T) {
	s := testStore(t)

	plain, err := s.CreateTask("plain", "", "goal", "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if plain.GroupID != "" {
		t.Errorf("GroupID = %q on an ordinary task, want empty", plain.GroupID)
	}

	sub, err := s.CreateTaskFrom(NewTask{Title: "sub", Goal: "goal", GroupID: "grp1", WorkspaceID: "ws1"})
	if err != nil {
		t.Fatalf("CreateTaskFrom: %v", err)
	}
	got, err := s.GetTask(sub.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.GroupID != "grp1" {
		t.Errorf("GroupID = %q, want grp1", got.GroupID)
	}
}
