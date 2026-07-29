package store

import (
	"database/sql"

	"fanoutd/internal/models"
)

// Claims record which task may write which path when several tasks share one
// workspace. They are what lets a broken-down idea run its subtasks in parallel
// against a single output directory instead of merging N directories afterward.
//
// The rule is one writer per path. Enforcement is the primary key on
// task_writes, not a lock: a losing claim is a failed insert, which surfaces to
// the agent as an ordinary tool error it can route around.

// Conflict reports a path that could not be claimed because another task holds
// it.
type Conflict struct {
	Path  string
	Owner string
}

// ClaimWrite records taskID as the sole writer of path. It returns the task
// already holding the path, or "" when the claim succeeded. Re-claiming a path
// this task already owns succeeds.
//
// First writer wins, which is deliberate: a path no one has claimed becomes the
// caller's. A breakdown cannot predict every file its subtasks will create, so
// an unplanned write is allowed and only a contested one is refused.
func (s *Store) ClaimWrite(workspaceID, path, taskID string) (string, error) {
	res, err := s.db.Exec(
		"INSERT OR IGNORE INTO task_writes (workspace_id, path, task_id) VALUES (?, ?, ?)",
		workspaceID, path, taskID,
	)
	if err != nil {
		return "", err
	}
	if n, err := res.RowsAffected(); err == nil && n == 1 {
		return "", nil
	}

	owner, err := s.WriteOwner(workspaceID, path)
	if err != nil {
		return "", err
	}
	if owner == taskID {
		return "", nil
	}
	return owner, nil
}

// WriteOwner reports the task holding path, or "" when it is unclaimed.
func (s *Store) WriteOwner(workspaceID, path string) (string, error) {
	var owner string
	err := s.db.QueryRow(
		"SELECT task_id FROM task_writes WHERE workspace_id = ? AND path = ?",
		workspaceID, path,
	).Scan(&owner)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return owner, err
}

// DeclareWrites claims every path up front, which is how a breakdown reserves
// its subtasks' outputs before any of them run. Paths that are already spoken
// for are returned as conflicts; the ones that were free stay claimed, so a
// caller rejecting the plan should call ReleaseClaims on the group.
func (s *Store) DeclareWrites(workspaceID, taskID string, paths []string) ([]Conflict, error) {
	conflicts := []Conflict{}
	for _, p := range paths {
		owner, err := s.ClaimWrite(workspaceID, p, taskID)
		if err != nil {
			return nil, err
		}
		if owner != "" {
			conflicts = append(conflicts, Conflict{Path: p, Owner: owner})
		}
	}
	return conflicts, nil
}

// DeclareReads records the paths a task expects to consume. Several tasks may
// read one path, so this never conflicts.
func (s *Store) DeclareReads(workspaceID, taskID string, paths []string) error {
	for _, p := range paths {
		_, err := s.db.Exec(
			"INSERT OR IGNORE INTO task_reads (workspace_id, path, task_id) VALUES (?, ?, ?)",
			workspaceID, p, taskID,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// OwnedPaths lists what a task may write. It is reported back to the agent
// whenever a claim is refused — knowing which files are its own is the
// actionable half of that error.
func (s *Store) OwnedPaths(workspaceID, taskID string) ([]string, error) {
	return s.claimPaths("task_writes", workspaceID, taskID)
}

// ReadPaths lists what a task declared it would consume.
func (s *Store) ReadPaths(workspaceID, taskID string) ([]string, error) {
	return s.claimPaths("task_reads", workspaceID, taskID)
}

func (s *Store) claimPaths(table, workspaceID, taskID string) ([]string, error) {
	rows, err := s.db.Query(
		"SELECT path FROM "+table+" WHERE workspace_id = ? AND task_id = ? ORDER BY path ASC",
		workspaceID, taskID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// Writers maps every claimed path in a workspace to the task that owns it. The
// scheduler joins it against each task's reads to derive the dependency edges,
// so the graph is a consequence of the file partition rather than a second
// thing the breakdown has to get right.
func (s *Store) Writers(workspaceID string) (map[string]string, error) {
	rows, err := s.db.Query(
		"SELECT path, task_id FROM task_writes WHERE workspace_id = ?", workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	writers := map[string]string{}
	for rows.Next() {
		var path, taskID string
		if err := rows.Scan(&path, &taskID); err != nil {
			return nil, err
		}
		writers[path] = taskID
	}
	return writers, rows.Err()
}

// GroupReads returns every task's declared reads in one query, keyed by task.
func (s *Store) GroupReads(workspaceID string) (map[string][]string, error) {
	rows, err := s.db.Query(
		"SELECT task_id, path FROM task_reads WHERE workspace_id = ? ORDER BY path ASC", workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reads := map[string][]string{}
	for rows.Next() {
		var taskID, path string
		if err := rows.Scan(&taskID, &path); err != nil {
			return nil, err
		}
		reads[taskID] = append(reads[taskID], path)
	}
	return reads, rows.Err()
}

// ActiveWriter reports the task still working on path, or "" when the path is
// unclaimed, held by the reader itself, or held by a task that has finished.
//
// It exists for reads the breakdown never declared — the model listing the
// workspace and opening whatever looks useful. Scheduling cannot order an edge
// it was never told about, so the read is allowed and annotated rather than
// refused: the file may simply be complete already, and blocking exploration
// costs more than a caveat.
func (s *Store) ActiveWriter(workspaceID, path, readerID string) (string, error) {
	var owner, status string
	err := s.db.QueryRow(
		`SELECT w.task_id, t.status FROM task_writes w
		 JOIN tasks t ON t.id = w.task_id
		 WHERE w.workspace_id = ? AND w.path = ?`,
		workspaceID, path,
	).Scan(&owner, &status)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if owner == readerID || status == models.StatusDone {
		return "", nil
	}
	return owner, nil
}

// TasksInGroup lists the subtasks of one breakdown, oldest first so a stable
// order survives into the schedule.
func (s *Store) TasksInGroup(groupID string) ([]models.Task, error) {
	rows, err := s.db.Query(
		"SELECT "+taskCols+" FROM tasks WHERE group_id = ? AND group_id != '' ORDER BY created_at ASC",
		groupID,
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

// ReleaseClaims drops every claim a task holds, freeing its paths for another
// task. Used when a task is deleted and when a rejected breakdown is unwound.
func (s *Store) ReleaseClaims(taskID string) error {
	if _, err := s.db.Exec("DELETE FROM task_writes WHERE task_id = ?", taskID); err != nil {
		return err
	}
	_, err := s.db.Exec("DELETE FROM task_reads WHERE task_id = ?", taskID)
	return err
}
