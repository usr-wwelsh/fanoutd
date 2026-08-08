package agent

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"fanoutd/internal/models"
	"fanoutd/internal/llm"
)

// readPageBytes is one read_file page. It stays under the tool-result budget in
// buildMessages so a page and its continuation notice survive into the next
// prompt intact - otherwise the model sees a truncated file with no way back.
const readPageBytes = 6000

// finishTool is not a workspace operation - the model calls it to end the run.
const finishTool = "finish"

// passTool and rejectTool end a review pass. They stand where finish stands for
// an author, and a review that ends without either of them is a review that
// reached no verdict.
const (
	passTool   = "pass"
	rejectTool = "reject"
)

// execTool runs a shell command in the workspace. Advertised only when a
// sandbox exists.
const execTool = "run_command"

// defaultWriteName catches a write_file call that omits the path. Losing the
// content over a missing filename costs more than picking one.
const defaultWriteName = "output.md"

// ToolCall is what the model asks for, either as a native tool call or in the
// JSON fallback body.
type ToolCall struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Old     string `json:"old"`
	New     string `json:"new"`
	Summary string `json:"summary"`
	Offset  int    `json:"offset"`
	Command string `json:"command"`
	// Findings carries a review's reasons for sending work back. It is the goal
	// the rework task is created with, so it is prose for another agent rather
	// than a note for the operator.
	Findings string `json:"findings"`
}

func toolString(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func toolDef(name, desc string, props map[string]any, required ...string) llm.Tool {
	if required == nil {
		required = []string{}
	}
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        name,
			Description: desc,
			Parameters: map[string]any{
				"type":       "object",
				"properties": props,
				"required":   required,
			},
		},
	}
}

// ToolDefs advertises the workspace tools to the model in native form.
// run_command appears only when a sandbox was built, so a host without working
// bubblewrap never offers the model a tool it will refuse.
func ToolDefs(sandboxed bool) []llm.Tool {
	str, def := toolString, toolDef

	path := str("Path relative to the workspace root.")
	tools := []llm.Tool{
		def("write_file", "Create or overwrite a file in the workspace. Use this for every deliverable - response text is not saved.",
			map[string]any{"path": path, "content": str("Full file contents.")}, "path", "content"),
		def("read_file", fmt.Sprintf("Read a file back from the workspace, up to %d bytes per call. If the result says bytes remain, call again with offset set to continue from there.", readPageBytes),
			map[string]any{
				"path":   path,
				"offset": map[string]any{"type": "integer", "description": "Byte offset to start reading from. Omit or 0 for the start of the file."},
			}, "path"),
		def("edit_file", "Replace the first occurrence of an exact string in a workspace file.",
			map[string]any{"path": path, "old": str("Exact text to replace."), "new": str("Replacement text.")}, "path", "old", "new"),
		def("delete_file", "Delete a file from the workspace.",
			map[string]any{"path": path}, "path"),
		def("list_files", "List the files currently in the workspace.",
			map[string]any{}),
		def(finishTool, "Call this once the goal is fully achieved and every deliverable has been written to a file.",
			map[string]any{"summary": str("What you produced, including the files you wrote.")}, "summary"),
	}

	if sandboxed {
		tools = append(tools, def(execTool,
			"Run a shell command in the workspace to build, test or inspect what you have written. "+
				"Runs in an isolated sandbox with no network access; only the workspace is writable. "+
				"Build artifacts belong outside the workspace — the toolchain is already configured to put them there. "+
				"Returns combined stdout and stderr with the exit status.",
			map[string]any{"command": str("Shell command line, e.g. \"go test ./...\" or \"cargo build\".")}, "command"))
	}
	return tools
}

// ReviewToolDefs is the reviewer's half of the same set: everything needed to
// inspect and execute the work, and nothing that changes it. A reviewer that can
// edit the workspace stops being a second opinion and becomes a second author.
func ReviewToolDefs(sandboxed bool) []llm.Tool {
	str, def := toolString, toolDef

	path := str("Path relative to the workspace root.")
	tools := []llm.Tool{
		def("read_file", fmt.Sprintf("Read a file, up to %d bytes per call. If the result says bytes remain, call again with offset set to continue from there.", readPageBytes),
			map[string]any{
				"path":   path,
				"offset": map[string]any{"type": "integer", "description": "Byte offset to start reading from. Omit or 0 for the start of the file."},
			}, "path"),
		def("list_files", "List the files in the workspace under review.",
			map[string]any{}),
	}
	if sandboxed {
		tools = append(tools, def(execTool,
			"Run a shell command against the work under review, to build it, test it, or execute it. "+
				"Runs in an isolated sandbox with no network access. "+
				"Returns combined stdout and stderr with the exit status.",
			map[string]any{"command": str("Shell command line, e.g. \"go test ./...\" or \"node index.js\".")}, "command"))
	}
	return append(tools, VerdictToolDefs()...)
}

// VerdictToolDefs is pass and reject with nothing beside them, for the turn a
// reviewer that has run out of steps is asked to decide on what it has already
// seen. Taking the reading tools away is the whole point of that turn: a model
// offered one more read will take it, and the step after that is the one it does
// not get.
func VerdictToolDefs() []llm.Tool {
	str, def := toolString, toolDef
	return []llm.Tool{
		def(passTool, "Accept the work. Call this only once you have checked every criterion and each one holds.",
			map[string]any{"summary": str("What you checked and how you checked it, criterion by criterion.")}, "summary"),
		def(rejectTool, "Send the work back. Call this when any criterion does not hold.",
			map[string]any{"findings": str("What is wrong and how to tell you have fixed it. Written for the agent that will do the rework, naming files and observed behaviour.")}, "findings"),
	}
}

// reviewTool reports whether a call is one the reviewer is allowed to make.
// Advertising a narrower set is not enough on its own: a model that has seen
// write_file in another life will still emit one, and Workspace.Exec would carry
// it out. So the verdict on what a reviewer may do is taken here, at the point
// of execution.
func reviewTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "read_file", "list_files", execTool:
		return true
	}
	return false
}

// mutatingTool reports whether a call changes the workspace itself. Those are
// judged on their arguments alone: writing the same bytes to the same path three
// times is a loop however much else has moved. Every other call is judged
// against the files it ran on, since its result is a function of them.
func mutatingTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "write_file", "edit_file", "delete_file":
		return true
	}
	return false
}

func (t *ToolCall) signature() string {
	// The command is part of the signature, so re-running an identical command
	// without changing anything counts against the repeat limit — which is the
	// right call: it is the shape of an agent stuck on a failing test.
	return strings.Join([]string{t.Name, t.Path, t.Content, t.Old, t.New, t.Command, strconv.Itoa(t.Offset)}, "\x00")
}

// Claims arbitrates which task may write which path when several tasks share a
// workspace. It is the interface half of store.Store's claim tables, kept here
// so the ownership rules can be tested without a database.
type Claims interface {
	// ClaimWrite records taskID as the sole writer of path, returning the task
	// that already holds it or "" when the claim succeeded.
	ClaimWrite(workspaceID, path, taskID string) (string, error)
	// OwnedPaths lists what taskID may already write.
	OwnedPaths(workspaceID, taskID string) ([]string, error)
	// ActiveWriter names the task still working on path, or "" when reading it
	// is safe.
	ActiveWriter(workspaceID, path, readerID string) (string, error)
}

// ownedInError caps how many of a task's own paths are listed when a claim is
// refused. The point is to orient the model, not to dump its whole manifest.
const ownedInError = 8

// Workspace is a sandbox rooted at <outputDir>/<workspaceID>. Tasks that
// continue an earlier run share one; so do the subtasks of a breakdown, which
// is why writes can be arbitrated.
type Workspace struct {
	root        string
	workspaceID string
	// taskID and claims are set only when a workspace is shared by tasks that
	// can run at once. A nil claims means no arbitration, which is the ordinary
	// single-task case and the behaviour every existing caller gets.
	taskID string
	claims Claims
	// sandbox is nil unless shell commands are available, which is what makes
	// withholding the tool and refusing the call the same decision.
	sandbox *Sandbox
}

func NewWorkspace(outputDir, workspaceID string) (*Workspace, error) {
	root, err := filepath.Abs(filepath.Join(outputDir, workspaceID))
	if err != nil {
		return nil, err
	}
	return &Workspace{root: root, workspaceID: workspaceID}, nil
}

// Owned returns a view of the workspace that writes as taskID and honours
// claims. The receiver is left alone, so a caller listing or deleting files
// keeps the unarbitrated view.
func (w *Workspace) Owned(taskID string, claims Claims) *Workspace {
	shared := *w
	shared.taskID = taskID
	shared.claims = claims
	return &shared
}

// Sandboxed returns a view of the workspace that can run shell commands as
// taskID. A nil sandbox leaves the workspace file-only.
func (w *Workspace) Sandboxed(taskID string, sb *Sandbox) *Workspace {
	shell := *w
	shell.taskID = taskID
	shell.sandbox = sb
	return &shell
}

func (w *Workspace) Root() string { return w.root }

// ResolvePath maps a workspace-relative path to an absolute one, rejecting
// anything that would escape the sandbox.
func (w *Workspace) ResolvePath(rel string) (string, error) { return w.resolve(rel) }

// resolve keeps every path inside the workspace root. The model is told the
// absolute workspace directory, so it routinely answers with an absolute path;
// those are folded back onto the root rather than rejected.
func (w *Workspace) resolve(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("path is required")
	}
	if filepath.IsAbs(rel) {
		clean := filepath.Clean(rel)
		switch inner, err := filepath.Rel(w.root, clean); {
		case err == nil && !strings.HasPrefix(inner, ".."):
			rel = inner
		case siblingTail(w.root, clean) != "":
			// The workspace root is a long random id and the model is shown it in
			// full, so it sometimes echoes back a near miss — a dropped character,
			// a truncation. That names a sibling workspace, and folding the whole
			// string in creates output/<id>/home/user/.../src/thing.js: a file
			// nobody asked for, at a path nothing else in the plan will read.
			// Reading it as the path within a workspace is what was meant.
			rel = siblingTail(w.root, clean)
		default:
			// Any other absolute path is read as workspace-relative instead of
			// escaping; the containment check below still applies.
			rel = strings.TrimPrefix(clean, string(os.PathSeparator))
		}
		if rel == "" || rel == "." {
			return "", fmt.Errorf("path is required")
		}
	}
	full := filepath.Clean(filepath.Join(w.root, rel))
	if full != w.root && !strings.HasPrefix(full, w.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes the workspace")
	}
	return full, nil
}

// siblingTail reads an absolute path that points into a different workspace
// under the same output directory and returns the part after that workspace's
// id, or "" when the path is not of that shape. The id itself is not checked
// against anything: whatever sits directly under the output directory is a
// workspace, and one that does not exist is a misspelling of the one that does.
func siblingTail(root, clean string) string {
	rest, err := filepath.Rel(filepath.Dir(root), clean)
	if err != nil || strings.HasPrefix(rest, "..") {
		return ""
	}
	_, tail, ok := strings.Cut(rest, string(os.PathSeparator))
	if !ok {
		return ""
	}
	return tail
}

// resolveOwned resolves rel and takes the write claim on it, returning the
// absolute path. Claims are keyed on the path relative to the root rather than
// on what the model typed, so "board.js", "./board.js" and the absolute form
// are one path and cannot be used to claim it three times.
func (w *Workspace) resolveOwned(rel string) (string, error) {
	full, err := w.resolve(rel)
	if err != nil {
		return "", err
	}
	if w.claims == nil {
		return full, nil
	}
	key, err := filepath.Rel(w.root, full)
	if err != nil {
		return "", err
	}
	if err := w.claim(key); err != nil {
		return "", err
	}
	return full, nil
}

// claim takes ownership of a path or explains who holds it. The refusal names
// the paths this task does own, since picking a different file is the only
// useful thing it can do next.
func (w *Workspace) claim(key string) error {
	owner, err := w.claims.ClaimWrite(w.workspaceID, key, w.taskID)
	if err != nil {
		return err
	}
	if owner == "" {
		return nil
	}
	return fmt.Errorf("%s belongs to task %s and is not yours to change.%s",
		key, shortID(owner), w.ownedSuffix())
}

func (w *Workspace) ownedSuffix() string {
	owned, err := w.claims.OwnedPaths(w.workspaceID, w.taskID)
	if err != nil || len(owned) == 0 {
		return " You have not claimed any files yet — write to a new path instead."
	}
	if len(owned) > ownedInError {
		owned = append(owned[:ownedInError:ownedInError], "...")
	}
	return " You own: " + strings.Join(owned, ", ") + "."
}

// shortID trims a task ID to the prefix the board displays, so a claim refusal
// reads the same as everything else the user sees.
func shortID(id string) string {
	if len(id) <= 7 {
		return id
	}
	return id[:7]
}

// Exec runs a tool call and returns a result string for the model.
func (w *Workspace) Exec(tc ToolCall) (string, error) {
	return w.ExecContext(context.Background(), tc)
}

// ExecContext is Exec bound to the run's context, so a stopped task kills the
// command it is waiting on instead of outliving itself.
func (w *Workspace) ExecContext(ctx context.Context, tc ToolCall) (string, error) {
	switch strings.ToLower(strings.TrimSpace(tc.Name)) {
	case "write_file":
		return w.writeFile(tc.Path, tc.Content)
	case "read_file":
		return w.readFile(tc.Path, tc.Offset)
	case "edit_file":
		return w.editFile(tc.Path, tc.Old, tc.New)
	case "delete_file":
		return w.deleteFile(tc.Path)
	case "list_files":
		return w.listFiles()
	case execTool:
		return w.runCommand(ctx, tc.Command)
	default:
		return "", fmt.Errorf("unknown tool %q (available: %s)", tc.Name, strings.Join(w.toolNames(), ", "))
	}
}

func (w *Workspace) toolNames() []string {
	names := []string{"write_file", "read_file", "edit_file", "delete_file", "list_files"}
	if w.sandbox != nil {
		names = append(names, execTool)
	}
	return names
}

// runCommand executes a shell command against the workspace and reconciles what
// it wrote with the claim table afterwards.
func (w *Workspace) runCommand(ctx context.Context, command string) (string, error) {
	if w.sandbox == nil {
		return "", fmt.Errorf("%s is not available on this server", execTool)
	}
	if err := os.MkdirAll(w.root, 0o755); err != nil {
		return "", err
	}

	before := w.stamps()
	out, err := w.sandbox.Run(ctx, w.root, w.taskID, command)
	if err != nil {
		return "", err
	}
	return out + w.reconcile(before), nil
}

// fileStamp is enough to notice a file changed without reading it back.
type fileStamp struct {
	size int64
	mod  time.Time
}

func (w *Workspace) stamps() map[string]fileStamp {
	if w.claims == nil {
		return nil
	}
	stamps := map[string]fileStamp{}
	entries, err := w.List()
	if err != nil {
		return stamps
	}
	for _, e := range entries {
		stamps[e.Path] = fileStamp{size: e.Size, mod: e.Modified}
	}
	return stamps
}

// reconcile takes claims on whatever the command wrote. Shell commands bypass
// resolveOwned entirely, so without this a subtask could overwrite a sibling's
// file through a build step and the one-writer rule would hold only for the
// tools that happen to go through write_file.
//
// A path this task actually created becomes its own, exactly as an unplanned
// write_file would. A path another task holds cannot be given back — the bytes
// are already on disk — so it is reported instead, which is the same shape of
// error a refused write_file returns and the only thing the model can act on.
func (w *Workspace) reconcile(before map[string]fileStamp) string {
	if w.claims == nil {
		return ""
	}
	after, err := w.List()
	if err != nil {
		return ""
	}

	violations := []string{}
	for _, e := range after {
		old, existed := before[e.Path]
		if existed && old.size == e.Size && old.mod.Equal(e.Modified) {
			continue
		}
		owner, err := w.claims.ClaimWrite(w.workspaceID, e.Path, w.taskID)
		if err == nil && owner != "" {
			violations = append(violations, fmt.Sprintf("%s (task %s)", e.Path, shortID(owner)))
		}
	}
	if len(violations) == 0 {
		return ""
	}
	if len(violations) > ownedInError {
		violations = append(violations[:ownedInError:ownedInError], "...")
	}
	return fmt.Sprintf("\n\n[warning: this command wrote files owned by other tasks: %s. "+
		"Those files are not yours to change — undo the change or work in a path you own.]",
		strings.Join(violations, ", "))
}

func (w *Workspace) writeFile(rel, content string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		rel = defaultWriteName
	}
	full, err := w.resolveOwned(rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s (%d bytes)", w.display(full), len(content)), nil
}

// display renders a resolved path the way the workspace actually holds it. The
// result is the only place the model learns where its file went, so echoing back
// what it typed hides every fold resolve performs: a write to an absolute path
// answered "wrote /home/...", and the read that followed went looking for
// exactly that.
func (w *Workspace) display(full string) string {
	rel, err := filepath.Rel(w.root, full)
	if err != nil {
		return full
	}
	return rel
}

func (w *Workspace) readFile(rel string, offset int) (string, error) {
	full, err := w.resolve(rel)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(data) {
		return "", fmt.Errorf("offset %d is past the end of %s (%d bytes)", offset, rel, len(data))
	}

	page := data[offset:]
	out := string(page)
	if len(page) > readPageBytes {
		next := offset + readPageBytes
		out = fmt.Sprintf("%s\n\n[showing bytes %d-%d of %d. %d bytes remain — call read_file with offset=%d to continue]",
			page[:readPageBytes], offset, next, len(data), len(data)-next, next)
	}
	return out + w.freshness(full), nil
}

// freshness warns when a file is still being written by a sibling subtask. The
// scheduler orders every dependency it was told about, so this only fires on a
// read the breakdown never declared — and a caveat is the right response there,
// since the file is very often finished and refusing the read would break the
// exploration the model does to orient itself.
func (w *Workspace) freshness(full string) string {
	if w.claims == nil {
		return ""
	}
	key, err := filepath.Rel(w.root, full)
	if err != nil {
		return ""
	}
	owner, err := w.claims.ActiveWriter(w.workspaceID, key, w.taskID)
	if err != nil || owner == "" {
		return ""
	}
	return fmt.Sprintf("\n\n[note: task %s is still working on %s, so this may not be its final content]",
		shortID(owner), key)
}

func (w *Workspace) editFile(rel, old, new string) (string, error) {
	if old == "" {
		return "", fmt.Errorf("edit_file requires a non-empty \"old\" string")
	}
	full, err := w.resolveOwned(rel)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	body := string(data)
	if !strings.Contains(body, old) {
		return "", fmt.Errorf("%s does not contain the \"old\" string", rel)
	}
	updated := strings.Replace(body, old, new, 1)
	if err := os.WriteFile(full, []byte(updated), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("edited %s (%d bytes)", w.display(full), len(updated)), nil
}

func (w *Workspace) deleteFile(rel string) (string, error) {
	full, err := w.resolveOwned(rel)
	if err != nil {
		return "", err
	}
	if full == w.root {
		return "", fmt.Errorf("cannot delete the workspace root")
	}
	if err := os.Remove(full); err != nil {
		return "", err
	}
	return fmt.Sprintf("deleted %s", w.display(full)), nil
}

// FileEntry lives in models because the CLI reports workspace listings too, and
// it cannot import this package without linking sqlite.
type FileEntry = models.FileEntry

func (w *Workspace) List() ([]FileEntry, error) {
	entries := []FileEntry{}
	if _, err := os.Stat(w.root); os.IsNotExist(err) {
		return entries, nil
	}
	err := filepath.Walk(w.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(w.root, path)
		if err != nil {
			return err
		}
		entries = append(entries, FileEntry{Path: rel, Abs: path, Size: info.Size(), Modified: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

// Fingerprint is a cheap summary of what the workspace holds. It exists so a
// command re-run against edited files can be told apart from one re-run against
// nothing: `go test` after a fix is progress, `go test` twice in a row is a
// loop, and the command line alone cannot distinguish them.
func (w *Workspace) Fingerprint() string {
	entries, err := w.List()
	if err != nil {
		return ""
	}
	h := fnv.New64a()
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", e.Path, e.Size, e.Modified.UnixNano())
	}
	return strconv.FormatUint(h.Sum64(), 36)
}

func (w *Workspace) listFiles() (string, error) {
	entries, err := w.List()
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "workspace is empty", nil
	}
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, fmt.Sprintf("%s (%d bytes)", e.Path, e.Size))
	}
	return strings.Join(lines, "\n"), nil
}
