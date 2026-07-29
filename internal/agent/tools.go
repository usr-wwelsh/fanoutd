package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"fanoutd/internal/models"
	"fanoutd/internal/openrouter"
)

// readPageBytes is one read_file page. It stays under the tool-result budget in
// buildMessages so a page and its continuation notice survive into the next
// prompt intact - otherwise the model sees a truncated file with no way back.
const readPageBytes = 6000

// finishTool is not a workspace operation - the model calls it to end the run.
const finishTool = "finish"

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
}

// ToolDefs advertises the workspace tools to the model in native form.
func ToolDefs() []openrouter.Tool {
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	def := func(name, desc string, props map[string]any, required ...string) openrouter.Tool {
		if required == nil {
			required = []string{}
		}
		return openrouter.Tool{
			Type: "function",
			Function: openrouter.ToolFunction{
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

	path := str("Path relative to the workspace root.")
	return []openrouter.Tool{
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
}

func (t *ToolCall) signature() string {
	return strings.Join([]string{t.Name, t.Path, t.Content, t.Old, t.New, strconv.Itoa(t.Offset)}, "\x00")
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
		if inner, err := filepath.Rel(w.root, clean); err == nil && !strings.HasPrefix(inner, "..") {
			rel = inner
		} else {
			// An absolute path outside the workspace is read as workspace-relative
			// instead of escaping it; the containment check below still applies.
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
	default:
		return "", fmt.Errorf("unknown tool %q (available: write_file, read_file, edit_file, delete_file, list_files)", tc.Name)
	}
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
	return fmt.Sprintf("wrote %s (%d bytes)", rel, len(content)), nil
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
	return fmt.Sprintf("edited %s (%d bytes)", rel, len(updated)), nil
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
	return fmt.Sprintf("deleted %s", rel), nil
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
