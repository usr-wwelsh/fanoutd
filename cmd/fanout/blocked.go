package main

import (
	"fmt"
	"io"
	"strings"

	"fanoutd/internal/models"
)

// This file exists so that finding out what needs a nudge costs one request and
// a handful of lines. `ls --status stopped`, then `show` on each to learn why,
// then `start` on each is three round trips per task and a screen of output for
// a question whose whole answer is "these two are stuck, on repetition".
//
// Every reason is already on the task row: an outright failure carries it in
// Error, and a conceded run carries it in the parenthetical of Summary. So the
// listing needs nothing but GET /api/tasks.

// reasonWidth keeps the last column to one line. The longest reason the agent
// writes is the repetition one, which fits.
const reasonWidth = 62

func cmdBlocked(e *env, args []string) error {
	fs := e.flags("blocked")
	all := fs.Bool("all", false, "include tasks parked in ideas or filed under finished")
	resume := fs.Bool("resume", false, "start every task listed")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fanout blocked [--resume] [--all] [--json]")
		fs.PrintDefaults()
	}
	if err := e.parse(fs, args); err != nil {
		return err
	}

	tasks, err := e.client.ListTasks(e.ctx)
	if err != nil {
		return e.describeErr(err)
	}
	stuck := blockedTasks(tasks, *all)

	if len(stuck) == 0 {
		if *asJSON {
			return writeJSON(e.out, []blockedRow{})
		}
		fmt.Fprintln(e.out, "nothing blocked")
		return nil
	}

	rows := make([]blockedRow, len(stuck))
	for i, t := range stuck {
		rows[i] = blockedRow{
			ID: t.ID, Title: t.Title, Column: t.Column,
			Status: t.Status, Reason: blockReason(t), Group: t.GroupID,
		}
	}

	if !*resume {
		if *asJSON {
			return writeJSON(e.out, rows)
		}
		printBlocked(e.out, rows)
		return nil
	}

	// Resume in listing order, and keep going past one that will not start:
	// a task the server refuses is not a reason to leave the rest stopped.
	var failed error
	for i := range rows {
		state, err := e.client.StartTask(e.ctx, rows[i].ID)
		if err != nil {
			rows[i].Resumed, failed = err.Error(), err
			continue
		}
		rows[i].Resumed = state.Status
	}
	if *asJSON {
		if err := writeJSON(e.out, rows); err != nil {
			return err
		}
	} else {
		printResumed(e.out, rows)
	}
	if failed != nil {
		return e.describeErr(failed)
	}
	return nil
}

// blockedRow is one stuck task, flattened. Nothing here needs a second request,
// which is the point of the command.
type blockedRow struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Column string `json:"column"`
	Status string `json:"status"`
	Reason string `json:"reason"`
	Group  string `json:"group_id,omitempty"`
	// Resumed is the status the start returned, or the error it returned, and
	// is set only by --resume.
	Resumed string `json:"resumed,omitempty"`
}

// blockedTasks selects the runs that stopped short of their goal. Both stopped
// and error qualify: which one a guard produces depends only on whether the
// workspace had files in it, and either way the run is over before the goal was
// met and resuming picks up from the existing trace.
//
// Column is the filter that matters. A conceded run is filed under finished as
// done, so anything still sitting in todo stopped for a reason nobody has dealt
// with — which is exactly the set worth resuming. Ideas and finished are where
// a task lands once someone has looked at it, so they need --all.
func blockedTasks(tasks []models.Task, all bool) []models.Task {
	out := make([]models.Task, 0, 4)
	for _, t := range tasks {
		if t.Status != models.StatusStopped && t.Status != models.StatusError {
			continue
		}
		if !all && t.Column != "todo" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// blockReason is the one line that says why, without opening the trace.
func blockReason(t models.Task) string {
	if r := strings.TrimSpace(t.Error); r != "" {
		return trimReason(r)
	}
	if r := concededReason(t.Summary); r != "" {
		return trimReason(r)
	}
	if t.Status == models.StatusStopped {
		return "stopped, no reason recorded"
	}
	return t.Status
}

// concededReason pulls the guard's wording back out of a concede summary, whose
// shape is "Stopped after N steps without calling finish (<reason>). ...".
func concededReason(summary string) string {
	open := strings.IndexByte(summary, '(')
	if open < 0 {
		return ""
	}
	shut := strings.LastIndexByte(summary, ')')
	if shut < open {
		return ""
	}
	return summary[open+1 : shut]
}

// trimReason drops what the row already says. "agent " is redundant on a board
// of agent runs, and the repetition guard quotes the action it saw repeated,
// which is detail for `show` rather than for a one-line listing.
func trimReason(r string) string {
	r = strings.TrimPrefix(strings.TrimSpace(r), "agent ")
	if i := strings.Index(r, `: "`); i > 0 {
		r = r[:i]
	}
	// The repetition guard's own wording. Repeating an action is the definition
	// of not progressing, so the clause only pushes the useful half off the row.
	r = strings.TrimSuffix(r, " without making progress")
	return clip(r, reasonWidth)
}

func printBlocked(w io.Writer, rows []blockedRow) {
	grouped := false
	for _, r := range rows {
		if r.Group != "" {
			grouped = true
			break
		}
	}
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		line := []string{shortID(r.ID), clip(r.Title, titleWidth)}
		if grouped {
			line = append(line, groupCell(r.Group))
		}
		out = append(out, append(line, r.Reason))
	}
	table(w, out)
	fmt.Fprintf(w, "\n%s — resume with `fanout blocked --resume`\n", plural(len(rows), "blocked task"))
}

func printResumed(w io.Writer, rows []blockedRow) {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{shortID(r.ID), clip(r.Title, titleWidth), r.Resumed})
	}
	table(w, out)
}

// groupCell marks a subtask with the group to pass to `fanout plan`, since a
// blocked subtask usually means its siblings are waiting on it.
func groupCell(id string) string {
	if id == "" {
		return "-"
	}
	return "group " + shortID(id)
}
