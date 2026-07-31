package agent

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fakeClaims is store.Store's claim tables without the database, so the
// ownership rules can be exercised on their own.
type fakeClaims struct {
	owners map[string]string // "workspace\x00path" -> task
	// active names tasks still running, so ActiveWriter can warn about a file
	// its owner has not finished with.
	active map[string]bool
	err    error
}

func newFakeClaims() *fakeClaims {
	return &fakeClaims{owners: map[string]string{}, active: map[string]bool{}}
}

func (f *fakeClaims) ActiveWriter(workspaceID, path, readerID string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	owner, held := f.owners[workspaceID+"\x00"+path]
	if !held || owner == readerID || !f.active[owner] {
		return "", nil
	}
	return owner, nil
}

func (f *fakeClaims) ClaimWrite(workspaceID, path, taskID string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	key := workspaceID + "\x00" + path
	if owner, held := f.owners[key]; held {
		if owner == taskID {
			return "", nil
		}
		return owner, nil
	}
	f.owners[key] = taskID
	return "", nil
}

func (f *fakeClaims) OwnedPaths(workspaceID, taskID string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	paths := []string{}
	for key, owner := range f.owners {
		ws, path, _ := strings.Cut(key, "\x00")
		if ws == workspaceID && owner == taskID {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func testWorkspace(t *testing.T) *Workspace {
	t.Helper()
	ws, err := NewWorkspace(t.TempDir(), "ws1")
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	return ws
}

func mustExec(t *testing.T, ws *Workspace, tc ToolCall) string {
	t.Helper()
	out, err := ws.Exec(tc)
	if err != nil {
		t.Fatalf("Exec(%s %s): %v", tc.Name, tc.Path, err)
	}
	return out
}

func TestUnarbitratedWorkspaceIsUnchanged(t *testing.T) {
	ws := testWorkspace(t)

	// The single-task case has no claims at all, so nothing is refused.
	mustExec(t, ws, ToolCall{Name: "write_file", Path: "a.md", Content: "one"})
	mustExec(t, ws, ToolCall{Name: "write_file", Path: "b.md", Content: "two"})
	mustExec(t, ws, ToolCall{Name: "edit_file", Path: "a.md", Old: "one", New: "1"})
	mustExec(t, ws, ToolCall{Name: "delete_file", Path: "b.md"})
}

func TestClaimRefusesAnotherTasksFile(t *testing.T) {
	claims := newFakeClaims()
	base := testWorkspace(t)
	a := base.Owned("taskAAAAAAA", claims)
	b := base.Owned("taskBBBBBBB", claims)

	mustExec(t, a, ToolCall{Name: "write_file", Path: "board.js", Content: "a"})
	mustExec(t, b, ToolCall{Name: "write_file", Path: "render.js", Content: "b"})

	_, err := b.Exec(ToolCall{Name: "write_file", Path: "board.js", Content: "clobbered"})
	if err == nil {
		t.Fatal("write to another task's file succeeded, want a refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, "board.js") || !strings.Contains(msg, "taskAAA") {
		t.Errorf("refusal %q does not name the path and its owner", msg)
	}
	// The actionable half: what this task may write instead.
	if !strings.Contains(msg, "render.js") {
		t.Errorf("refusal %q does not list the task's own paths", msg)
	}

	// The file is untouched.
	got := mustExec(t, a, ToolCall{Name: "read_file", Path: "board.js"})
	if got != "a" {
		t.Errorf("content = %q, want %q", got, "a")
	}
}

func TestClaimGatesEditAndDelete(t *testing.T) {
	claims := newFakeClaims()
	base := testWorkspace(t)
	a := base.Owned("taskA", claims)
	b := base.Owned("taskB", claims)

	mustExec(t, a, ToolCall{Name: "write_file", Path: "board.js", Content: "hello"})

	// edit_file replacing the first occurrence of a string is exactly the
	// operation that corrupts a file under concurrent writers.
	if _, err := b.Exec(ToolCall{Name: "edit_file", Path: "board.js", Old: "hello", New: "bye"}); err == nil {
		t.Error("edit on another task's file succeeded, want a refusal")
	}
	if _, err := b.Exec(ToolCall{Name: "delete_file", Path: "board.js"}); err == nil {
		t.Error("delete of another task's file succeeded, want a refusal")
	}

	if got := mustExec(t, a, ToolCall{Name: "read_file", Path: "board.js"}); got != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
}

func TestClaimKeyIsCanonical(t *testing.T) {
	claims := newFakeClaims()
	base := testWorkspace(t)
	a := base.Owned("taskA", claims)
	b := base.Owned("taskB", claims)

	mustExec(t, a, ToolCall{Name: "write_file", Path: "src/board.js", Content: "a"})

	// Three spellings of one path must not be three separate claims, or a
	// second task walks straight past the constraint.
	for _, spelling := range []string{
		"./src/board.js",
		"src/../src/board.js",
		filepath.Join(base.Root(), "src", "board.js"),
	} {
		if _, err := b.Exec(ToolCall{Name: "write_file", Path: spelling, Content: "b"}); err == nil {
			t.Errorf("write to %q succeeded, want a refusal", spelling)
		}
	}

	if got := mustExec(t, a, ToolCall{Name: "read_file", Path: "src/board.js"}); got != "a" {
		t.Errorf("content = %q, want %q", got, "a")
	}
}

func TestUnclaimedPathGoesToFirstWriter(t *testing.T) {
	claims := newFakeClaims()
	base := testWorkspace(t)
	a := base.Owned("taskA", claims)
	b := base.Owned("taskB", claims)

	// A breakdown cannot predict every file, so an unplanned path is allowed
	// and simply becomes the writer's.
	mustExec(t, b, ToolCall{Name: "write_file", Path: "helper.js", Content: "b"})
	if _, err := a.Exec(ToolCall{Name: "write_file", Path: "helper.js", Content: "a"}); err == nil {
		t.Error("second writer of an unplanned path succeeded, want a refusal")
	}
}

func TestReadsAreNotArbitrated(t *testing.T) {
	claims := newFakeClaims()
	base := testWorkspace(t)
	a := base.Owned("taskA", claims)
	b := base.Owned("taskB", claims)

	mustExec(t, a, ToolCall{Name: "write_file", Path: "schema.json", Content: "{}"})

	// Consuming a sibling's output is the normal case, not a conflict.
	if got := mustExec(t, b, ToolCall{Name: "read_file", Path: "schema.json"}); got != "{}" {
		t.Errorf("read = %q, want %q", got, "{}")
	}
	// And everyone sees the whole workspace.
	if got := mustExec(t, b, ToolCall{Name: "list_files"}); !strings.Contains(got, "schema.json") {
		t.Errorf("list_files = %q, want it to include a sibling's file", got)
	}
}

func TestOwnedLeavesReceiverUnarbitrated(t *testing.T) {
	claims := newFakeClaims()
	base := testWorkspace(t)
	a := base.Owned("taskA", claims)

	mustExec(t, a, ToolCall{Name: "write_file", Path: "board.js", Content: "a"})

	// The server lists and deletes files through the plain workspace; that view
	// must not start claiming paths on the tasks' behalf.
	mustExec(t, base, ToolCall{Name: "delete_file", Path: "board.js"})
}

func TestResolveContainsPaths(t *testing.T) {
	ws := testWorkspace(t)
	root := ws.Root()

	escapes := []string{"../outside.md", "a/../../outside.md", "", "   "}
	for _, rel := range escapes {
		if _, err := ws.resolve(rel); err == nil {
			t.Errorf("resolve(%q) succeeded, want an error", rel)
		}
	}

	// The model is told the absolute workspace directory and routinely answers
	// with one, so those fold back onto the root rather than being rejected.
	inside := map[string]string{
		"a.md":                             filepath.Join(root, "a.md"),
		"./a.md":                           filepath.Join(root, "a.md"),
		"src/../a.md":                      filepath.Join(root, "a.md"),
		filepath.Join(root, "a.md"):        filepath.Join(root, "a.md"),
		filepath.Join(root, "src", "a.md"): filepath.Join(root, "src", "a.md"),
		// An absolute path outside the workspace is read as workspace-relative.
		"/etc/passwd": filepath.Join(root, "etc", "passwd"),
		// A near miss at the workspace id — the model is shown the root in full
		// and sometimes echoes it back short a character. That names a sibling
		// workspace, and folding the whole string in would bury the file at
		// <root>/home/user/.../src/a.md, where nothing else in the plan reads it.
		filepath.Join(filepath.Dir(root), "0bad", "src", "a.md"): filepath.Join(root, "src", "a.md"),
	}
	for rel, want := range inside {
		got, err := ws.resolve(rel)
		if err != nil {
			t.Errorf("resolve(%q): %v", rel, err)
			continue
		}
		if got != want {
			t.Errorf("resolve(%q) = %q, want %q", rel, got, want)
		}
	}
}
