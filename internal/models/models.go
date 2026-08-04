package models

import "time"

const (
	StatusIdle    = "idle"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusStopped = "stopped"
	StatusError   = "error"
)

// The board's columns. ColumnReview holds work an agent has signed off on but
// nobody has checked; with review switched off nothing is ever filed there, and
// a run goes from ColumnTodo straight to ColumnFinished as it always did.
const (
	ColumnIdeas    = "ideas"
	ColumnTodo     = "todo"
	ColumnReview   = "review"
	ColumnFinished = "finished"
)

// What a review made of a run. An empty verdict is the third case and the one
// the review column exists for: nobody has answered for this work yet.
const (
	VerdictPassed   = "passed"
	VerdictRejected = "rejected"
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
	// Criteria is what the output must do, one per line, settled before the work
	// starts. It is what review checks against — a reviewer holding only the goal
	// is grading the author's own homework, since the only other objective signal
	// it has is the tests the author wrote. Empty for a task created by hand,
	// where the goal has to stand in for it.
	Criteria string `json:"criteria"`
	// ReviewRound counts how many times this line of work has already been sent
	// back. It rides on the task rather than on the workspace so that a rework
	// task carries it forward and the bounce cannot run forever.
	ReviewRound int `json:"review_round"`
	// Verdict is what a review made of this run: passed, rejected, or empty for
	// work no reviewer has answered for. It duplicates a line the same verdict
	// appended to Summary, and earns that: a board colours several hundred cards
	// per poll and cannot read prose to find out which of them were accepted,
	// while the summary has to stay readable on its own for the CLI and for
	// anyone reading the row a year later.
	Verdict string `json:"verdict"`
	// VerdictNote is what the reviewer said — its findings when it sent the work
	// back, and what it checked when it passed it. It is the one part of a review
	// that has to be read rather than counted.
	VerdictNote string `json:"verdict_note"`
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
	// Owned reports that this task wrote the file, as opposed to a sibling
	// subtask it shares the workspace with. A solo task owns everything in its
	// workspace, so it is only meaningful within a breakdown — where without it a
	// listing credits all five subtasks' output to whichever one you asked.
	Owned bool `json:"owned"`
}

// ToolExchange is one native tool call together with what running it returned.
//
// It is recorded because the next request has to replay the call the way the
// provider expects — an assistant turn carrying tool_calls, then a "tool" message
// keyed on the same id. Replaying only the prose around a call, which is all
// Response holds, hands the model a transcript in which it can see that it wrote
// a file but not what it wrote.
type ToolExchange struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Arguments is the JSON object string the model sent, kept verbatim so the
	// replayed call is the one it actually made.
	Arguments string `json:"arguments"`
	Result    string `json:"result"`
}

type TraceStep struct {
	ID         int    `json:"id"`
	TaskID     string `json:"task_id"`
	StepNumber int    `json:"step_number"`
	Action     string `json:"action"`
	Prompt     string `json:"prompt"`
	Response   string `json:"response"`
	ToolName   string `json:"tool_name"`
	ToolResult string `json:"tool_result"`
	// Calls holds every tool call this step made, in order. It is empty for a
	// step that used the JSON fallback protocol, for the bookkeeping steps a run
	// records around itself, and for rows written before calls were kept at all —
	// ToolName and ToolResult still summarise it, so anything that only displays
	// a step needs no knowledge of this field.
	Calls     []ToolExchange `json:"calls,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
}
