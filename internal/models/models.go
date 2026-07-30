package models

import "time"

const (
	StatusIdle    = "idle"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusStopped = "stopped"
	StatusError   = "error"
)

type Task struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Goal        string `json:"goal"`
	Column      string `json:"column"`
	Summary     string `json:"summary"`
	FinishFlag  bool   `json:"finish_flag"`
	Status      string `json:"status"`
	Error       string `json:"error"`
	// Model overrides the configured default for this task. Empty means default.
	Model string `json:"model"`
	// WorkspaceID names the output directory. Several tasks can share one, which
	// is how a new goal picks up where an earlier run left off.
	WorkspaceID string `json:"workspace_id"`
	// ParentID records the task this one was continued or retried from.
	ParentID string `json:"parent_id"`
	// GroupID ties together the subtasks of one broken-down idea. It is what
	// distinguishes siblings that may run at once — and therefore need their
	// writes arbitrated — from a task that merely continues an earlier one and
	// is free to edit everything it inherited. Empty means neither.
	GroupID   string    `json:"group_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GroupPlan is a breakdown's resolved schedule together with the state of its
// subtasks. It is what /api/groups/:id/plan returns, and it lives here rather
// than in the agent package so the CLI can decode it without linking sqlite.
//
// Waves and Deps hold task IDs. A wave is everything that may run at once;
// execution does not follow them in lockstep, so treat them as the shape of the
// graph rather than as a timeline.
type GroupPlan struct {
	GroupID string `json:"group_id"`
	// Idea is the wording the group was split from, recovered from the subtasks
	// rather than stored. Empty for a group this server did not break down.
	Idea  string              `json:"idea,omitempty"`
	Waves [][]string          `json:"waves"`
	Deps  map[string][]string `json:"deps"`
	// Writes and Reads are the file partition Deps was derived from, keyed by
	// task ID. An edge says B waits for A; these say which path made it wait.
	Writes  map[string][]string `json:"writes"`
	Reads   map[string][]string `json:"reads"`
	Tasks   []Task              `json:"tasks"`
	Running bool                `json:"running"`
}

// BreakdownResult is what POST /api/breakdown returns.
//
// Fallback is non-empty when the idea could not be partitioned and was created
// as one ordinary task instead — the safe floor. GroupID and Plan are empty in
// exactly that case, and Tasks holds the single task.
type BreakdownResult struct {
	GroupID  string     `json:"group_id,omitempty"`
	Tasks    []Task     `json:"tasks"`
	Plan     *GroupPlan `json:"plan,omitempty"`
	Fallback string     `json:"fallback,omitempty"`
	Started  bool       `json:"started"`
}

// SeedFile is one file placed in a workspace before anything runs, so an agent
// starts with material rather than an empty directory. Content travels with the
// request: the client reads the local path, which is what makes seeding work
// against a board on another machine.
type SeedFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// FileEntry is one file in a task workspace, as returned by /api/tasks/:id/files.
type FileEntry struct {
	Path string `json:"path"`
	// Abs is the on-disk location, which the UI turns into a file:// link.
	Abs      string    `json:"abs"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type TraceStep struct {
	ID         int       `json:"id"`
	TaskID     string    `json:"task_id"`
	StepNumber int       `json:"step_number"`
	Action     string    `json:"action"`
	Prompt     string    `json:"prompt"`
	Response   string    `json:"response"`
	ToolName   string    `json:"tool_name"`
	ToolResult string    `json:"tool_result"`
	Timestamp  time.Time `json:"timestamp"`
}
