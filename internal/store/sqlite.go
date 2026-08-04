package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"fanoutd/internal/models"
	_ "modernc.org/sqlite"
)

const taskCols = `id, title, description, goal, criteria, review_round, column, summary, finish_flag, status, error, model, workspace_id, parent_id, group_id, created_at, updated_at`

type Store struct {
	db *sql.DB
}

// dsn attaches the pragmas that make concurrent writers safe.
//
// busy_timeout is the important one and has to ride on the DSN rather than a
// one-off Exec, because it is per-connection and database/sql pools several.
// Without it a second writer fails instantly with SQLITE_BUSY instead of
// waiting its turn — which never mattered while runs were effectively serial,
// and breaks immediately once the subtasks of one breakdown record their
// traces at the same time.
//
// WAL is persistent in the file, so setting it here is belt and braces, but it
// is what lets readers keep working while a writer holds the lock.
func dsn(path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
}

func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	if err := initSchema(db); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func initSchema(db *sql.DB) error {
	tasksSQL := `CREATE TABLE IF NOT EXISTS tasks (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		goal TEXT NOT NULL DEFAULT '',
		criteria TEXT NOT NULL DEFAULT '',
		review_round INTEGER NOT NULL DEFAULT 0,
		column TEXT NOT NULL DEFAULT 'ideas',
		summary TEXT NOT NULL DEFAULT '',
		finish_flag INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'idle',
		error TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		workspace_id TEXT NOT NULL DEFAULT '',
		parent_id TEXT NOT NULL DEFAULT '',
		group_id TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);`

	traceSQL := `CREATE TABLE IF NOT EXISTS trace_steps (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id TEXT NOT NULL,
		step_number INTEGER NOT NULL,
		action TEXT NOT NULL,
		prompt TEXT NOT NULL,
		response TEXT NOT NULL,
		tool_name TEXT NOT NULL DEFAULT '',
		tool_result TEXT NOT NULL DEFAULT '',
		calls TEXT NOT NULL DEFAULT '',
		timestamp DATETIME NOT NULL,
		FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
	);`

	// task_writes is the whole collision story for parallel subtasks sharing a
	// workspace: the primary key means a path has exactly one writer, so a
	// second task claiming it gets a failed insert rather than a corrupted file.
	writesSQL := `CREATE TABLE IF NOT EXISTS task_writes (
		workspace_id TEXT NOT NULL,
		path TEXT NOT NULL,
		task_id TEXT NOT NULL,
		PRIMARY KEY (workspace_id, path)
	);`

	// Reads are non-exclusive. They exist so a scheduler can derive the
	// dependency between a task that reads a path and the task that writes it.
	readsSQL := `CREATE TABLE IF NOT EXISTS task_reads (
		workspace_id TEXT NOT NULL,
		path TEXT NOT NULL,
		task_id TEXT NOT NULL,
		PRIMARY KEY (workspace_id, path, task_id)
	);`

	for _, stmt := range []string{tasksSQL, traceSQL, writesSQL, readsSQL} {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}

	migrations := []struct{ table, column, def string }{
		{"tasks", "status", "TEXT NOT NULL DEFAULT 'idle'"},
		{"tasks", "error", "TEXT NOT NULL DEFAULT ''"},
		{"tasks", "model", "TEXT NOT NULL DEFAULT ''"},
		{"tasks", "workspace_id", "TEXT NOT NULL DEFAULT ''"},
		{"tasks", "parent_id", "TEXT NOT NULL DEFAULT ''"},
		{"tasks", "group_id", "TEXT NOT NULL DEFAULT ''"},
		{"tasks", "criteria", "TEXT NOT NULL DEFAULT ''"},
		{"tasks", "review_round", "INTEGER NOT NULL DEFAULT 0"},
		{"trace_steps", "tool_name", "TEXT NOT NULL DEFAULT ''"},
		{"trace_steps", "tool_result", "TEXT NOT NULL DEFAULT ''"},
		{"trace_steps", "calls", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, m := range migrations {
		if err := addColumnIfMissing(db, m.table, m.column, m.def); err != nil {
			return err
		}
	}

	// Tasks created before workspaces could be shared own theirs outright.
	if _, err := db.Exec("UPDATE tasks SET workspace_id = id WHERE workspace_id = ''"); err != nil {
		return err
	}

	// Tasks finished before the status column existed.
	if _, err := db.Exec("UPDATE tasks SET status='done' WHERE finish_flag=1 AND status='idle'"); err != nil {
		return err
	}

	// A run cannot survive a restart; anything left "running" was interrupted.
	_, err := db.Exec("UPDATE tasks SET status='stopped' WHERE status='running'")
	return err
}

func addColumnIfMissing(db *sql.DB, table, column, def string) error {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, def))
	return err
}

// NewTask describes a task to insert. WorkspaceID empty means the task gets a
// workspace of its own; set it to share another task's output directory.
type NewTask struct {
	Title       string
	Description string
	Goal        string
	// Criteria is what review will check the output against, one per line.
	Criteria string
	// ReviewRound carries forward from the task this one reworks, so the bounce
	// between todo and review is bounded across the chain rather than per task.
	ReviewRound int
	Model       string
	WorkspaceID string
	ParentID    string
	GroupID     string
}

func (s *Store) CreateTask(title, description, goal, model string) (*models.Task, error) {
	return s.CreateTaskFrom(NewTask{Title: title, Description: description, Goal: goal, Model: model})
}

func (s *Store) CreateTaskFrom(nt NewTask) (*models.Task, error) {
	id := generateID()
	workspaceID := nt.WorkspaceID
	if workspaceID == "" {
		workspaceID = id
	}
	now := time.Now().UTC()
	_, err := s.db.Exec(
		"INSERT INTO tasks ("+taskCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		id, nt.Title, nt.Description, nt.Goal, nt.Criteria, nt.ReviewRound,
		"ideas", "", false, models.StatusIdle, "",
		nt.Model, workspaceID, nt.ParentID, nt.GroupID, now, now,
	)
	if err != nil {
		return nil, err
	}
	return s.GetTask(id)
}

func (s *Store) GetTask(id string) (*models.Task, error) {
	row := s.db.QueryRow("SELECT "+taskCols+" FROM tasks WHERE id = ?", id)
	t := &models.Task{}
	err := scanTask(row.Scan, t)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return t, err
}

// scanTask keeps the column order in one place, since taskCols is shared.
func scanTask(scan func(...any) error, t *models.Task) error {
	return scan(&t.ID, &t.Title, &t.Description, &t.Goal, &t.Criteria, &t.ReviewRound,
		&t.Column, &t.Summary, &t.FinishFlag, &t.Status, &t.Error, &t.Model,
		&t.WorkspaceID, &t.ParentID, &t.GroupID, &t.CreatedAt, &t.UpdatedAt)
}

func (s *Store) ListTasks() ([]models.Task, error) {
	rows, err := s.db.Query("SELECT " + taskCols + " FROM tasks ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []models.Task{}
	for rows.Next() {
		t := models.Task{}
		if err := scanTask(rows.Scan, &t); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func (s *Store) UpdateTask(id string, title, description, goal, column, summary, model string, finishFlag bool) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		"UPDATE tasks SET title=?, description=?, goal=?, column=?, summary=?, model=?, finish_flag=?, updated_at=? WHERE id=?",
		title, description, goal, column, summary, model, finishFlag, now, id,
	)
	return err
}

func (s *Store) SetTaskColumn(id, column string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec("UPDATE tasks SET column=?, updated_at=? WHERE id=?", column, now, id)
	return err
}

func (s *Store) SetTaskStatus(id, status, errMsg string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec("UPDATE tasks SET status=?, error=?, updated_at=? WHERE id=?", status, errMsg, now, id)
	return err
}

// ReclaimRunningTasks demotes tasks left at "running" to "stopped" and reports
// how many it moved. A run lives only in the server's memory, so any task still
// marked running at startup belongs to a process that is gone — a crash, a kill
// -9, or a shutdown that ran out of time. Without this they stay running
// forever: nothing is left to write their final status, and start refuses them.
func (s *Store) ReclaimRunningTasks() (int, error) {
	now := time.Now().UTC()
	res, err := s.db.Exec(
		"UPDATE tasks SET status=?, error=?, updated_at=? WHERE status=?",
		models.StatusStopped, "interrupted by a server restart", now, models.StatusRunning,
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// CountTasksInWorkspace reports how many tasks still point at a workspace, so a
// delete can tell an orphaned output directory from a shared one.
func (s *Store) CountTasksInWorkspace(workspaceID string) (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM tasks WHERE workspace_id = ?", workspaceID).Scan(&n)
	return n, err
}

func (s *Store) DeleteTask(id string) error {
	if _, err := s.db.Exec("DELETE FROM trace_steps WHERE task_id = ?", id); err != nil {
		return err
	}
	if err := s.ReleaseClaims(id); err != nil {
		return err
	}
	_, err := s.db.Exec("DELETE FROM tasks WHERE id = ?", id)
	return err
}

func (s *Store) ListTraceSteps(taskID string) ([]models.TraceStep, error) {
	rows, err := s.db.Query(
		"SELECT id, task_id, step_number, action, prompt, response, tool_name, tool_result, calls, timestamp FROM trace_steps WHERE task_id = ? ORDER BY id ASC",
		taskID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	steps := []models.TraceStep{}
	for rows.Next() {
		ts := models.TraceStep{}
		var calls string
		err := rows.Scan(&ts.ID, &ts.TaskID, &ts.StepNumber, &ts.Action, &ts.Prompt, &ts.Response, &ts.ToolName, &ts.ToolResult, &calls, &ts.Timestamp)
		if err != nil {
			return nil, err
		}
		// A row whose calls will not decode is one written by an older build or
		// hand-edited; it still has its prose, which is what the replay falls
		// back to, so it is not worth failing the whole trace over.
		if calls != "" {
			json.Unmarshal([]byte(calls), &ts.Calls)
		}
		steps = append(steps, ts)
	}
	return steps, rows.Err()
}

// TraceEntry is one row to append. A step can make several tool calls at once,
// which is more than an argument list wants to say.
type TraceEntry struct {
	TaskID     string
	Step       int
	Action     string
	Prompt     string
	Response   string
	ToolName   string
	ToolResult string
	Calls      []models.ToolExchange
}

func (s *Store) AddTrace(e TraceEntry) error {
	calls := ""
	if len(e.Calls) > 0 {
		encoded, err := json.Marshal(e.Calls)
		if err != nil {
			return err
		}
		calls = string(encoded)
	}
	now := time.Now().UTC()
	_, err := s.db.Exec(
		"INSERT INTO trace_steps (task_id, step_number, action, prompt, response, tool_name, tool_result, calls, timestamp) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		e.TaskID, e.Step, e.Action, e.Prompt, e.Response, e.ToolName, e.ToolResult, calls, now,
	)
	return err
}

// AddTraceStep records a step that made no replayable tool call: the JSON
// fallback protocol, and the bookkeeping a run writes around itself.
func (s *Store) AddTraceStep(taskID string, stepNumber int, action, prompt, response, toolName, toolResult string) error {
	return s.AddTrace(TraceEntry{
		TaskID: taskID, Step: stepNumber, Action: action, Prompt: prompt,
		Response: response, ToolName: toolName, ToolResult: toolResult,
	})
}

func (s *Store) ClearTrace(taskID string) error {
	_, err := s.db.Exec("DELETE FROM trace_steps WHERE task_id = ?", taskID)
	return err
}

// ClearFinishFlag withdraws the "this task is finished" mark. The loop reads
// that flag every step as a stop signal, so it has to be cleared when a run
// starts or the run ends on its first step — which is what made a conceded task
// impossible to resume, since conceding files the task through SetTaskFinished.
func (s *Store) ClearFinishFlag(id string) error {
	_, err := s.db.Exec(
		"UPDATE tasks SET finish_flag=0, updated_at=? WHERE id=?", time.Now().UTC(), id,
	)
	return err
}

// settle files a run that ended with output: its summary, the finish mark the
// loop reads as a stop signal, and done status. Only the column differs between
// the two ways that can happen, and separating them is what lets "the agent
// stopped" and "the work was accepted" stop being the same event.
func (s *Store) settle(id, column, summary string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		"UPDATE tasks SET column=?, summary=?, finish_flag=?, status=?, error='', updated_at=? WHERE id=?",
		column, summary, true, models.StatusDone, now, id,
	)
	return err
}

// SetTaskFinished accepts the work: the run is over and nothing further is owed.
func (s *Store) SetTaskFinished(id, summary string) error {
	return s.settle(id, models.ColumnFinished, summary)
}

// SetTaskInReview parks a finished run in front of a reviewer. The task is done
// in the sense that no agent is working on it; whether it is finished is the
// question the review answers.
func (s *Store) SetTaskInReview(id, summary string) error {
	return s.settle(id, models.ColumnReview, summary)
}

// TasksAwaitingReview lists the runs parked in front of a reviewer that no
// verdict was ever recorded for. A verdict is delivered by the goroutine that
// ran the task, so a process that goes away between a run settling and its
// review finishing leaves the work here: done, unjudged, and invisible to
// `fanout blocked`, which lists what stopped rather than what was never judged.
//
// Anything the review did reach has moved on or carries an error, so done in the
// review column is exactly the set still owed an answer.
func (s *Store) TasksAwaitingReview() ([]models.Task, error) {
	rows, err := s.db.Query(
		"SELECT "+taskCols+" FROM tasks WHERE column = ? AND status = ? ORDER BY created_at ASC",
		models.ColumnReview, models.StatusDone,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []models.Task{}
	for rows.Next() {
		t := models.Task{}
		if err := scanTask(rows.Scan, &t); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// SetTaskSummary rewrites the summary alone, for a verdict that changes what is
// known about a run without moving it.
func (s *Store) SetTaskSummary(id, summary string) error {
	_, err := s.db.Exec(
		"UPDATE tasks SET summary=?, updated_at=? WHERE id=?", summary, time.Now().UTC(), id,
	)
	return err
}
