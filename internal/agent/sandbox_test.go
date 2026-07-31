package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testSandbox builds a real sandbox or skips. The point of these tests is what
// bubblewrap actually enforces, so faking it would test nothing.
func testSandbox(t *testing.T, cfg SandboxConfig) *Sandbox {
	t.Helper()
	if cfg.StateDir == "" {
		cfg.StateDir = t.TempDir()
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	sb, err := NewSandbox(cfg)
	if err != nil {
		t.Skipf("no usable sandbox: %v", err)
	}
	return sb
}

func run(t *testing.T, sb *Sandbox, dir, command string) string {
	t.Helper()
	out, err := sb.Run(context.Background(), dir, "task1", command)
	if err != nil {
		t.Fatalf("Run(%q): %v", command, err)
	}
	return out
}

func TestSandboxRunsInWorkspace(t *testing.T) {
	sb := testSandbox(t, SandboxConfig{})
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "in.txt"), []byte("hello"), 0o644)

	out := run(t, sb, dir, "cat in.txt && echo made > out.txt")
	if !strings.Contains(out, "hello") {
		t.Fatalf("workspace file not readable: %q", out)
	}
	if !strings.Contains(out, "[exit status 0]") {
		t.Fatalf("missing exit status: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err != nil {
		t.Fatalf("command did not write into the workspace: %v", err)
	}
}

// TestSandboxWorkspaceIsAtItsHostPath is a regression test for a run that lost
// work silently. The prompt tells the model the workspace's absolute host path
// and the file tools accept it, so the shell has to as well: with the workspace
// bound at /work instead, `cd <host path>` failed, the model concluded the
// directory did not exist, and `mkdir -p <host path> && cd <host path> && go mod
// init` then succeeded against a directory inside the jail's own root that
// evaporated when the command exited — perfect output, no file.
func TestSandboxWorkspaceIsAtItsHostPath(t *testing.T) {
	sb := testSandbox(t, SandboxConfig{})
	dir := t.TempDir()

	out := run(t, sb, dir, "pwd")
	if !strings.Contains(out, dir) {
		t.Fatalf("commands do not start in the workspace's host path: %q", out)
	}

	// The absolute path the model is given has to work verbatim in the shell.
	out = run(t, sb, dir, "cd "+dir+" && echo made > built.txt && pwd")
	if !strings.Contains(out, "[exit status 0]") {
		t.Fatalf("cd to the workspace path failed: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "built.txt")); err != nil {
		t.Fatalf("work done at the absolute path did not persist: %v", err)
	}
}

func TestSandboxReportsFailureWithoutErroring(t *testing.T) {
	sb := testSandbox(t, SandboxConfig{})

	// A failing build's output is the useful part, so a non-zero exit is a
	// result and not a tool error.
	out := run(t, sb, t.TempDir(), "echo boom >&2; exit 3")
	if !strings.Contains(out, "boom") {
		t.Fatalf("stderr not captured: %q", out)
	}
	if !strings.Contains(out, "[exit status 3]") {
		t.Fatalf("exit status not reported: %q", out)
	}
}

func TestSandboxHasNoNetworkByDefault(t *testing.T) {
	sb := testSandbox(t, SandboxConfig{})

	out := run(t, sb, t.TempDir(), "getent hosts example.com || echo NONET")
	if !strings.Contains(out, "NONET") {
		t.Fatalf("expected name resolution to fail: %q", out)
	}
}

func TestSandboxClearsTheEnvironment(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-secret-value")
	sb := testSandbox(t, SandboxConfig{})

	// Anything the command can read ends up in the trace and from there in the
	// next prompt, so the key must not be in scope at all.
	out := run(t, sb, t.TempDir(), "env")
	if strings.Contains(out, "sk-secret-value") || strings.Contains(out, "OPENROUTER") {
		t.Fatalf("API key leaked into the sandbox: %q", out)
	}
}

func TestSandboxCannotWriteTheHost(t *testing.T) {
	sb := testSandbox(t, SandboxConfig{})
	outside := filepath.Join(t.TempDir(), "host.txt")

	out := run(t, sb, t.TempDir(), "echo pwned > "+outside+" || echo REFUSED; touch /usr/pwned || echo RO")
	if !strings.Contains(out, "REFUSED") {
		t.Fatalf("wrote outside the workspace: %q", out)
	}
	if !strings.Contains(out, "RO") {
		t.Fatalf("/usr was not read-only: %q", out)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("a host file outside the workspace was created")
	}
}

func TestSandboxBuildArtifactsStayOutOfTheWorkspace(t *testing.T) {
	sb := testSandbox(t, SandboxConfig{})
	dir := t.TempDir()

	// The toolchain cache variables point at /build and /cache, which are bound
	// outside the workspace, so a build never turns its intermediates into
	// deliverables.
	out := run(t, sb, dir, `echo "$CARGO_TARGET_DIR $GOCACHE"; touch "$GOCACHE/x" && echo CACHE_OK`)
	if !strings.Contains(out, "CACHE_OK") {
		t.Fatalf("shared cache not writable: %q", out)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("workspace polluted by build dirs: %v", entries)
	}
}

func TestSandboxTimeoutKillsTheCommand(t *testing.T) {
	sb := testSandbox(t, SandboxConfig{Timeout: 1 * time.Second})

	start := time.Now()
	out := run(t, sb, t.TempDir(), "echo starting; sleep 60")
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("timeout did not fire promptly: %s", elapsed)
	}
	if !strings.Contains(out, "time limit") {
		t.Fatalf("timeout not reported: %q", out)
	}
	// Output produced before the kill is still worth returning.
	if !strings.Contains(out, "starting") {
		t.Fatalf("partial output lost: %q", out)
	}
}

func TestSandboxCancellationStopsWaiting(t *testing.T) {
	sb := testSandbox(t, SandboxConfig{Timeout: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		sb.Run(ctx, t.TempDir(), "task1", "sleep 60")
	}()
	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("cancelling the run did not stop the command")
	}
}

func TestExecSlotsAreReleasedAndRespectContext(t *testing.T) {
	sb := testSandbox(t, SandboxConfig{MaxExec: 1, Timeout: time.Minute})

	// A stopped run must not sit in the queue holding a scheduler slot.
	sb.slots <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := sb.Run(ctx, t.TempDir(), "task1", "true"); err == nil {
		t.Fatal("expected a queued command to give up when its run is cancelled")
	}

	<-sb.slots
	if out := run(t, sb, t.TempDir(), "echo free"); !strings.Contains(out, "free") {
		t.Fatalf("slot not usable after release: %q", out)
	}
}

func TestShellToolHiddenWithoutSandbox(t *testing.T) {
	for _, tool := range ToolDefs(false) {
		if tool.Function.Name == execTool {
			t.Fatal("run_command advertised without a sandbox")
		}
	}

	found := false
	for _, tool := range ToolDefs(true) {
		if tool.Function.Name == execTool {
			found = true
		}
	}
	if !found {
		t.Fatal("run_command missing when a sandbox exists")
	}
}

func TestShellToolRefusedWithoutSandbox(t *testing.T) {
	ws := testWorkspace(t)

	// Withholding the definition and refusing the call are the same decision, so
	// a model that asks for the tool anyway gets an error rather than a shell.
	if _, err := ws.Exec(ToolCall{Name: execTool, Command: "echo hi"}); err == nil {
		t.Fatal("expected run_command to be refused without a sandbox")
	}
}

func TestShellWritesTakeClaims(t *testing.T) {
	sb := testSandbox(t, SandboxConfig{})
	claims := newFakeClaims()
	ws := testWorkspace(t).Sandboxed("task1", sb).Owned("task1", claims)

	if _, err := ws.Exec(ToolCall{Name: execTool, Command: "echo built > server.bin"}); err != nil {
		t.Fatalf("run_command: %v", err)
	}

	// A shell command bypasses resolveOwned, so the claim has to be taken after
	// the fact or the one-writer rule would only hold for write_file.
	owner, _ := claims.ClaimWrite("ws1", "server.bin", "task2")
	if owner != "task1" {
		t.Fatalf("shell write did not claim the path, owner=%q", owner)
	}
}

func TestShellReportsWritesOwnedByAnotherTask(t *testing.T) {
	sb := testSandbox(t, SandboxConfig{})
	claims := newFakeClaims()
	claims.ClaimWrite("ws1", "shared.txt", "task2")

	ws := testWorkspace(t).Sandboxed("task1", sb).Owned("task1", claims)
	os.WriteFile(filepath.Join(ws.Root(), "shared.txt"), []byte("theirs"), 0o644)
	// Stamps are second-resolution on some filesystems; make the change unmissable.
	time.Sleep(1100 * time.Millisecond)

	out, err := ws.Exec(ToolCall{Name: execTool, Command: "echo mine > shared.txt"})
	if err != nil {
		t.Fatalf("run_command: %v", err)
	}
	if !strings.Contains(out, "shared.txt") || !strings.Contains(out, "owned by other tasks") {
		t.Fatalf("clobbering a sibling's file was not reported: %q", out)
	}
}

func TestShellUntouchedFilesAreNotClaimed(t *testing.T) {
	sb := testSandbox(t, SandboxConfig{})
	claims := newFakeClaims()
	ws := testWorkspace(t).Sandboxed("task1", sb).Owned("task1", claims)
	os.WriteFile(filepath.Join(ws.Root(), "untouched.txt"), []byte("x"), 0o644)

	if _, err := ws.Exec(ToolCall{Name: execTool, Command: "ls -la"}); err != nil {
		t.Fatalf("run_command: %v", err)
	}

	// Reading the workspace must not quietly claim everything in it.
	if owner, _ := claims.ClaimWrite("ws1", "untouched.txt", "task2"); owner != "" {
		t.Fatalf("a file the command only listed was claimed by %q", owner)
	}
}

// TestSandboxCompilesAndRuns is the whole point of the feature: a toolchain the
// host has installed is usable inside the jail with no per-language support in
// fanoutd. Go stands in for any of them because this repo already needs it.
func TestSandboxCompilesAndRuns(t *testing.T) {
	sb := testSandbox(t, SandboxConfig{Timeout: 2 * time.Minute})
	dir := t.TempDir()

	if out := run(t, sb, dir, "command -v go || echo MISSING"); strings.Contains(out, "MISSING") {
		t.Skip("no go toolchain visible in the sandbox")
	}

	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module smoke\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"built and ran\") }\n"), 0o644)

	out := run(t, sb, dir, "go build -o /build/smoke . && /build/smoke")
	if !strings.Contains(out, "built and ran") {
		t.Fatalf("go build/run failed in the sandbox: %q", out)
	}

	// The binary went to /build, so the workspace still holds only source.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("build leaked artifacts into the workspace: %v", entries)
	}
}

// TestCommandIsRecordedInTheTrace guards the other half of that run: native
// tool call arguments are not replayed, so a command that is not in the action
// line is not anywhere, and the model reads back output it cannot attribute.
func TestCommandIsRecordedInTheTrace(t *testing.T) {
	action := describeCall(ToolCall{Name: execTool, Command: "go test ./..."})
	if !strings.Contains(action, "go test ./...") {
		t.Fatalf("command missing from the trace action: %q", action)
	}

	long := describeCall(ToolCall{Name: execTool, Command: strings.Repeat("x", 500)})
	if len(long) > 260 {
		t.Fatalf("command not truncated in the trace action: %d chars", len(long))
	}
}

func TestLoggedActionKeepsTheCommand(t *testing.T) {
	tc := &ToolCall{Name: execTool, Command: "go test ./..."}

	// The model usually writes its own action text, which is exactly when
	// describeCall does not run and the command would otherwise be lost.
	got := loggedAction("Let me run the tests again", tc)
	if !strings.Contains(got, "Let me run the tests again") || !strings.Contains(got, "go test ./...") {
		t.Fatalf("model prose and command not both recorded: %q", got)
	}

	// A synthesized label already names it; saying it twice helps nobody.
	if got := loggedAction(describeCall(*tc), tc); strings.Count(got, "go test") != 1 {
		t.Fatalf("command duplicated in the action: %q", got)
	}

	if got := loggedAction("just thinking", nil); got != "just thinking" {
		t.Fatalf("non-command action rewritten: %q", got)
	}
}

// TestFingerprintSeparatesRerunsFromLoops is a regression test for a run that
// was killed while it was converging: the agent fixed what the tests caught,
// re-ran them, and the repeat guard aborted it for issuing the same command
// three times. A command has to be judged against the files it runs on.
func TestFingerprintSeparatesRerunsFromLoops(t *testing.T) {
	ws := testWorkspace(t)
	os.MkdirAll(ws.Root(), 0o755)

	before := ws.Fingerprint()
	if before != ws.Fingerprint() {
		t.Fatal("fingerprint is unstable with no change")
	}

	mustExec(t, ws, ToolCall{Name: "write_file", Path: "main.go", Content: "package main"})
	afterWrite := ws.Fingerprint()
	if afterWrite == before {
		t.Fatal("a new file did not change the fingerprint — a real fix would look like a loop")
	}

	// Second-resolution mtimes on some filesystems would hide a same-size edit.
	time.Sleep(1100 * time.Millisecond)
	mustExec(t, ws, ToolCall{Name: "edit_file", Path: "main.go", Old: "main", New: "manx"})
	if ws.Fingerprint() == afterWrite {
		t.Fatal("an edit did not change the fingerprint")
	}
}

func TestCapOutputKeepsHeadAndTail(t *testing.T) {
	head := strings.Repeat("A", 100)
	tail := strings.Repeat("Z", 100)
	out := capOutput(head + strings.Repeat("m", execOutputBytes*2) + tail)

	if len(out) > execOutputBytes+200 {
		t.Fatalf("output not capped: %d bytes", len(out))
	}
	if !strings.HasPrefix(out, head) {
		t.Fatal("head dropped")
	}
	if !strings.HasSuffix(out, tail) {
		t.Fatal("tail dropped — build failures live at the end of the output")
	}
	if !strings.Contains(out, "bytes omitted") {
		t.Fatal("truncation not signposted")
	}
}

func TestCapOutputLeavesShortOutputAlone(t *testing.T) {
	if got := capOutput("small"); got != "small" {
		t.Fatalf("capOutput mangled short output: %q", got)
	}
}
