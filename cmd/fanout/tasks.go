package main

import (
	"fmt"
	"strings"
	"sync"

	"fanoutd/internal/models"
)

func cmdAdd(e *env, args []string) error {
	fs := e.flags("add")
	goal := fs.String("goal", "", "what the agent should achieve; \"-\" reads stdin")
	desc := fs.String("desc", "", "extra detail passed to the agent")
	model := fs.String("model", "", "override the server's default model")
	start := fs.Bool("start", false, "start the agent loop immediately")
	watch := fs.Bool("watch", false, "with --start, follow the run until it ends")
	seeds := seedFlag(fs)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fanout add <title> [--goal ...] [--seed path] [--start] [--model ...]")
		fs.PrintDefaults()
	}
	if err := e.parse(fs, args); err != nil {
		return err
	}

	title := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if title == "" {
		fs.Usage()
		return fmt.Errorf("a title is required")
	}

	g := strings.TrimSpace(*goal)
	if g == "-" {
		read, err := readStdin()
		if err != nil {
			return err
		}
		g = read
	}
	// A task with no goal is an idea card, which is a real thing to create;
	// starting one is not.
	if g == "" && *start {
		return fmt.Errorf("--start needs a goal (--goal ... or --goal - to read stdin)")
	}

	// Read before the task exists, so a bad path costs nothing on the board.
	seed, err := collectSeed(*seeds)
	if err != nil {
		return err
	}

	nt := clientNewTask(title, *desc, g, *model)
	nt.Seed = seed
	task, err := e.client.CreateTask(e.ctx, nt)
	if err != nil {
		return e.describeErr(err)
	}
	fmt.Fprintf(e.out, "%s  %s\n", shortID(task.ID), task.Title)
	if note := describeSeed(seed); note != "" {
		fmt.Fprintln(e.out, note)
	}

	if !*start {
		return nil
	}
	state, err := e.client.StartTask(e.ctx, task.ID)
	if err != nil {
		return e.describeErr(err)
	}
	fmt.Fprintf(e.out, "started (%s)\n", state.Status)
	if *watch {
		return watchTask(e, task.ID)
	}
	return nil
}

func cmdLs(e *env, args []string) error {
	fs := e.flags("ls")
	col := fs.String("col", "", "only this column: ideas, todo, review, finished")
	status := fs.String("status", "", "only this status: idle, running, done, stopped, error")
	asJSON := fs.Bool("json", false, "machine-readable output")
	plain := fs.Bool("plain", false, "skip the per-task step and file counts (one request instead of many)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fanout ls [--col todo] [--status running] [--json]")
		fs.PrintDefaults()
	}
	if err := e.parse(fs, args); err != nil {
		return err
	}

	tasks, err := e.client.ListTasks(e.ctx)
	if err != nil {
		return e.describeErr(err)
	}
	tasks = filterTasks(tasks, *col, *status)

	if len(tasks) == 0 {
		if !*asJSON {
			fmt.Fprintln(e.out, "no tasks")
			return nil
		}
		return writeJSON(e.out, []taskRow{})
	}

	rows := make([]taskRow, len(tasks))
	for i, t := range tasks {
		rows[i] = taskRow{Task: t}
	}
	if !*plain {
		e.fillDetail(rows)
	}

	if *asJSON {
		return writeJSON(e.out, rows)
	}
	printTaskRows(e.out, rows, nil)
	return nil
}

// fillDetail fetches the trace and file list for each row. The API has no bulk
// endpoint for this, so it is one request pair per task, run concurrently and
// bounded. A row whose detail cannot be fetched still prints.
func (e *env) fillDetail(rows []taskRow) {
	const parallel = 8
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup

	for i := range rows {
		wg.Add(1)
		go func(r *taskRow) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if steps, err := e.client.Trace(e.ctx, r.Task.ID); err == nil {
				r.Steps = len(steps)
				if len(steps) > 0 {
					r.Last = &steps[len(steps)-1]
				}
			}
			if files, err := e.client.Files(e.ctx, r.Task.ID); err == nil {
				// What the task wrote, not what is in the workspace. Subtasks of
				// one breakdown share a workspace, so counting the listing gave
				// every row of a group the same total.
				r.Files = len(ownedFiles(files))
			}
		}(&rows[i])
	}
	wg.Wait()
}

func filterTasks(tasks []models.Task, col, status string) []models.Task {
	col, status = strings.ToLower(strings.TrimSpace(col)), strings.ToLower(strings.TrimSpace(status))
	if col == "" && status == "" {
		return tasks
	}
	out := make([]models.Task, 0, len(tasks))
	for _, t := range tasks {
		if col != "" && t.Column != col {
			continue
		}
		if status != "" && t.Status != status {
			continue
		}
		out = append(out, t)
	}
	return out
}

// showSteps is how many trailing steps `show` prints. Enough to see what the
// agent is doing, few enough to stay glanceable.
const showSteps = 5

func cmdShow(e *env, args []string) error {
	fs := e.flags("show")
	asJSON := fs.Bool("json", false, "machine-readable output")
	n := fs.Int("last", showSteps, "how many trailing steps to print")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fanout show <id> [--last N] [--json]")
		fs.PrintDefaults()
	}
	id, err := e.resolve(fs, args)
	if err != nil {
		return e.describeErr(err)
	}

	state, err := e.client.Status(e.ctx, id)
	if err != nil {
		return e.describeErr(err)
	}
	steps, err := e.client.Trace(e.ctx, id)
	if err != nil {
		return e.describeErr(err)
	}
	files, err := e.client.Files(e.ctx, id)
	if err != nil {
		return e.describeErr(err)
	}

	task := state.Task
	if *asJSON {
		return writeJSON(e.out, map[string]any{
			"task": task, "running": state.Running,
			"steps": len(steps), "files": files,
			"trace": lastSteps(steps, *n),
		})
	}

	fmt.Fprintf(e.out, "%s  %s\n", task.ID, task.Title)
	meta := [][]string{
		{"column", task.Column},
		{"status", statusLine(state)},
	}
	if task.Model != "" {
		meta = append(meta, []string{"model", task.Model})
	}
	if task.Description != "" {
		meta = append(meta, []string{"details", clip(task.Description, 96)})
	}
	if task.Goal != "" {
		meta = append(meta, []string{"goal", clip(task.Goal, 96)})
	}
	if task.Summary != "" {
		meta = append(meta, []string{"summary", clip(task.Summary, 96)})
	}
	if task.Error != "" {
		meta = append(meta, []string{"error", clip(task.Error, 96)})
	}
	if task.ParentID != "" {
		meta = append(meta, []string{"parent", shortID(task.ParentID)})
	}
	// The only place a group id surfaces, and the way back to `fanout plan`.
	if task.GroupID != "" {
		meta = append(meta, []string{"group", shortID(task.GroupID)})
	}
	table(e.out, meta)

	// Only this task's own files, so a subtask is not shown its siblings' output
	// as if it had produced it. The rest of the shared workspace is a count and a
	// pointer to `fanout files --all`, which is what you want when the thing you
	// mean to open belongs to another subtask.
	mine := ownedFiles(files)
	if len(mine) > 0 {
		fmt.Fprintf(e.out, "\n%s:\n", plural(len(mine), "file"))
		rows := make([][]string, 0, len(mine))
		for _, f := range mine {
			rows = append(rows, []string{"  " + f.Path, humanSize(f.Size)})
		}
		table(e.out, rows)
	}
	if shared := len(files) - len(mine); shared > 0 {
		fmt.Fprintf(e.out, "\n%s from sibling subtasks in the shared workspace (fanout files %s --all)\n",
			plural(shared, "file"), shortID(task.ID))
	}

	if len(steps) > 0 {
		tail := lastSteps(steps, *n)
		fmt.Fprintf(e.out, "\n%s, last %d:\n", plural(len(steps), "step"), len(tail))
		printTrace(e.out, tail)
	}

	if task.Status == models.StatusError {
		return errTaskFailed
	}
	return nil
}

func statusLine(state *runState) string {
	s := state.Task.Status
	if state.Running {
		return s + " (live)"
	}
	if s == models.StatusRunning {
		// The row says running but no loop is attached — the server was
		// restarted mid-run. Saying so beats printing a lie.
		return s + " (no loop attached; start it again to resume)"
	}
	return s
}

func lastSteps(steps []models.TraceStep, n int) []models.TraceStep {
	if n <= 0 || n >= len(steps) {
		return steps
	}
	return steps[len(steps)-n:]
}

func cmdStart(e *env, args []string) error {
	fs := e.flags("start")
	watch := fs.Bool("watch", false, "follow the run until it ends")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fanout start <id> [--watch]")
		fs.PrintDefaults()
	}
	id, err := e.resolve(fs, args)
	if err != nil {
		return e.describeErr(err)
	}
	state, err := e.client.StartTask(e.ctx, id)
	if err != nil {
		return e.describeErr(err)
	}
	fmt.Fprintf(e.out, "%s  %s  %s\n", shortID(id), state.Task.Title, state.Status)
	if *watch {
		return watchTask(e, id)
	}
	return nil
}

func cmdStop(e *env, args []string) error {
	fs := e.flags("stop")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fanout stop <id>   (a task, or a breakdown's group)")
		fs.PrintDefaults()
	}
	id, err := e.resolve(fs, args)
	if err != nil {
		// A group id belongs to no task, so it never resolves as one. Falling
		// through to groups is what lets one verb stop either — and a group id
		// shares no prefix with its subtasks, so there is nothing to confuse.
		// Bad flags or a missing argument never dialled, so they stop here.
		if e.client == nil || fs.NArg() < 1 {
			return e.describeErr(err)
		}
		groupID, groupErr := resolveGroupID(e.ctx, e.client, fs.Arg(0))
		if groupErr != nil {
			return e.describeErr(err)
		}
		plan, err := e.client.StopGroup(e.ctx, groupID)
		if err != nil {
			return e.describeErr(err)
		}
		fmt.Fprintf(e.out, "%s  %s\n", shortID(groupID), summarizePlan(plan))
		printPlan(e.out, plan)
		return nil
	}

	state, err := e.client.StopTask(e.ctx, id)
	if err != nil {
		return e.describeErr(err)
	}
	fmt.Fprintf(e.out, "%s  %s  %s\n", shortID(id), state.Task.Title, state.Status)
	return nil
}

func cmdMv(e *env, args []string) error {
	fs := e.flags("mv")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fanout mv <id> <ideas|todo|review|finished>")
		fs.PrintDefaults()
	}
	id, err := e.resolve(fs, args)
	if err != nil {
		return e.describeErr(err)
	}
	if fs.NArg() < 2 {
		fs.Usage()
		return fmt.Errorf("a destination column is required")
	}
	task, err := e.client.MoveTask(e.ctx, id, strings.ToLower(fs.Arg(1)))
	if err != nil {
		return e.describeErr(err)
	}
	fmt.Fprintf(e.out, "%s  %s  → %s\n", shortID(task.ID), task.Title, task.Column)
	return nil
}

func cmdRm(e *env, args []string) error {
	fs := e.flags("rm")
	keep := fs.Bool("keep-files", false, "leave the workspace directory on disk")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fanout rm <id> [--keep-files]")
		fs.PrintDefaults()
	}
	id, err := e.resolve(fs, args)
	if err != nil {
		return e.describeErr(err)
	}
	// Read the title before deleting so the confirmation names what went.
	task, err := e.client.GetTask(e.ctx, id)
	if err != nil {
		return e.describeErr(err)
	}
	if err := e.client.DeleteTask(e.ctx, id, *keep); err != nil {
		return e.describeErr(err)
	}
	suffix := ""
	if *keep {
		suffix = " (files kept)"
	}
	fmt.Fprintf(e.out, "deleted %s  %s%s\n", shortID(id), task.Title, suffix)
	return nil
}

func cmdContinue(e *env, args []string) error {
	fs := e.flags("continue")
	goal := fs.String("goal", "", "the new goal; \"-\" reads stdin")
	title := fs.String("title", "", "title for the new task (default: the old one, numbered)")
	desc := fs.String("desc", "", "extra detail passed to the agent")
	model := fs.String("model", "", "override the model (default: inherit)")
	start := fs.Bool("start", false, "start the new task immediately")
	watch := fs.Bool("watch", false, "with --start, follow the run until it ends")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fanout continue <id> --goal ... [--start]")
		fs.PrintDefaults()
	}
	id, err := e.resolve(fs, args)
	if err != nil {
		return e.describeErr(err)
	}

	g := strings.TrimSpace(*goal)
	if g == "-" {
		read, err := readStdin()
		if err != nil {
			return err
		}
		g = read
	}
	if g == "" {
		fs.Usage()
		return fmt.Errorf("--goal is required")
	}

	f := followup(*title, *desc, g, *model, *start)
	task, err := e.client.ContinueTask(e.ctx, id, f)
	if err != nil {
		return e.describeErr(err)
	}
	fmt.Fprintf(e.out, "%s  %s  (workspace of %s)\n", shortID(task.ID), task.Title, shortID(id))
	if *start && *watch {
		return watchTask(e, task.ID)
	}
	return nil
}

func cmdRetry(e *env, args []string) error {
	fs := e.flags("retry")
	model := fs.String("model", "", "override the model (default: inherit)")
	start := fs.Bool("start", false, "start the new task immediately")
	watch := fs.Bool("watch", false, "with --start, follow the run until it ends")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fanout retry <id> [--model ...] [--start]")
		fs.PrintDefaults()
	}
	id, err := e.resolve(fs, args)
	if err != nil {
		return e.describeErr(err)
	}
	task, err := e.client.RetryTask(e.ctx, id, followup("", "", "", *model, *start))
	if err != nil {
		return e.describeErr(err)
	}
	fmt.Fprintf(e.out, "%s  %s  (clean workspace)\n", shortID(task.ID), task.Title)
	if *start && *watch {
		return watchTask(e, task.ID)
	}
	return nil
}

func cmdModels(e *env, args []string) error {
	fs := e.flags("models")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fanout models [--json]")
		fs.PrintDefaults()
	}
	if err := e.parse(fs, args); err != nil {
		return err
	}
	list, err := e.client.ListModels(e.ctx)
	if err != nil {
		return e.describeErr(err)
	}
	if *asJSON {
		return writeJSON(e.out, list)
	}
	rows := make([][]string, 0, len(list.Models))
	for _, m := range list.Models {
		marker := " "
		if m.ID == list.Default {
			marker = "*"
		}
		rows = append(rows, []string{marker, m.ID, clip(m.Name, 48)})
	}
	table(e.out, rows)
	return nil
}
