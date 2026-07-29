package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"fanoutd/internal/client"
	"fanoutd/internal/models"
)

func cmdBreakdown(e *env, args []string) error {
	fs := e.flags("breakdown")
	title := fs.String("title", "", "title for the fallback task, if the idea cannot be split")
	model := fs.String("model", "", "override the server's default model")
	start := fs.Bool("start", false, "run the schedule immediately")
	watch := fs.Bool("watch", false, "with --start, follow every subtask until the group ends")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `usage: fanout breakdown "<idea>" [--start] [--watch]`)
		fs.PrintDefaults()
	}
	if err := e.parse(fs, args); err != nil {
		return err
	}

	idea := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if idea == "-" {
		read, err := readStdin()
		if err != nil {
			return err
		}
		idea = read
	}
	if idea == "" {
		fs.Usage()
		return fmt.Errorf(`an idea is required (or "-" to read stdin)`)
	}

	result, err := e.client.Breakdown(e.ctx, client.Idea{
		Idea: idea, Title: *title, Model: *model, Start: *start,
	})
	if err != nil {
		return e.describeErr(err)
	}

	// The fallback is the normal outcome for an idea that does not divide, not
	// an error, so it prints as a note above an ordinary task line.
	if result.Fallback != "" {
		fmt.Fprintf(e.out, "%s\n", result.Fallback)
		if len(result.Tasks) == 0 {
			return nil
		}
		task := result.Tasks[0]
		fmt.Fprintf(e.out, "%s  %s\n", shortID(task.ID), task.Title)
		if *start && *watch {
			return watchTask(e, task.ID)
		}
		return nil
	}

	fmt.Fprintf(e.out, "%s  %s\n", shortID(result.GroupID), summarizePlan(result.Plan))
	printPlan(e.out, result.Plan)
	if *start && *watch {
		return watchGroup(e, result.GroupID, watchInterval)
	}
	if !*start {
		fmt.Fprintf(e.out, "\nstart it with `fanout plan %s --start`\n", shortID(result.GroupID))
	}
	return nil
}

func cmdPlan(e *env, args []string) error {
	fs := e.flags("plan")
	asJSON := fs.Bool("json", false, "machine-readable output")
	start := fs.Bool("start", false, "run the schedule")
	watch := fs.Bool("watch", false, "follow every subtask until the group ends")
	interval := fs.Duration("interval", watchInterval, "poll period while watching")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fanout plan <group-id> [--start] [--watch] [--json]")
		fs.PrintDefaults()
	}
	if err := e.parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return fmt.Errorf("a group id or prefix is required")
	}
	id, err := resolveGroupID(e.ctx, e.client, fs.Arg(0))
	if err != nil {
		return e.describeErr(err)
	}

	plan, err := e.client.GroupPlan(e.ctx, id)
	if err != nil {
		return e.describeErr(err)
	}
	if *start {
		if plan, err = e.client.StartGroup(e.ctx, id); err != nil {
			return e.describeErr(err)
		}
	}

	if *asJSON {
		return writeJSON(e.out, plan)
	}
	fmt.Fprintf(e.out, "%s  %s\n", shortID(id), summarizePlan(plan))
	printPlan(e.out, plan)
	if *watch {
		return watchGroup(e, id, *interval)
	}
	return nil
}

func summarizePlan(plan *models.GroupPlan) string {
	if plan == nil {
		return "no plan"
	}
	summary := fmt.Sprintf("%s in %s", plural(len(plan.Tasks), "subtask"), plural(len(plan.Waves), "wave"))
	if plan.Running {
		return summary + ", running"
	}
	return summary
}

// printPlan lays the group out by wave, which is the shape of the dependency
// graph rather than a timetable: execution starts each subtask as soon as its
// own dependencies are done, so a later wave can begin before an earlier one has
// emptied.
func printPlan(w io.Writer, plan *models.GroupPlan) {
	if plan == nil {
		return
	}
	byID := map[string]models.Task{}
	for _, t := range plan.Tasks {
		byID[t.ID] = t
	}

	rows := [][]string{}
	for i, wave := range plan.Waves {
		for j, id := range wave {
			label := ""
			if j == 0 {
				label = fmt.Sprintf("  wave %d", i+1)
			}
			task := byID[id]
			rows = append(rows, []string{
				label, shortID(id), clip(task.Title, titleWidth), task.Status, planDetail(task),
			})
		}
	}
	table(w, rows)
}

// planDetail is the one thing worth carrying next to a subtask's status: what it
// produced, or why it did not.
func planDetail(task models.Task) string {
	switch task.Status {
	case models.StatusError:
		return clip(firstLine(task.Error), detailWidth)
	case models.StatusDone:
		return clip(firstLine(task.Summary), detailWidth)
	default:
		return ""
	}
}

// watchGroup follows a whole schedule, printing each subtask as it settles. Like
// watchTaskEvery, the exit code is the point: a group with any failed subtask
// fails the command.
func watchGroup(e *env, groupID string, interval time.Duration) error {
	settled := map[string]bool{}
	for {
		plan, err := e.client.GroupPlan(e.ctx, groupID)
		if err != nil {
			return e.describeErr(err)
		}

		for _, task := range plan.Tasks {
			if settled[task.ID] || !terminalStatus(task.Status) {
				continue
			}
			settled[task.ID] = true
			line := fmt.Sprintf("%s  %s  %s", shortID(task.ID), pad(clip(task.Title, titleWidth), titleWidth), task.Status)
			if detail := planDetail(task); detail != "" {
				line += "  " + detail
			}
			fmt.Fprintln(e.out, line)
		}

		if done, err := groupSettled(e, plan); done {
			return err
		}

		select {
		case <-e.ctx.Done():
			fmt.Fprintf(os.Stderr, "\nstopped watching; %s is still running on %s\n", shortID(groupID), e.cfg.server)
			return context.Canceled
		case <-time.After(interval):
		}
	}
}

// groupSettled reports whether the schedule has finished, and with what outcome.
//
// The schedule outlives its individual subtasks — the server holds the group
// open from StartGroup until the last one ends — so "not running with work
// left" means nobody started it. Polling that forever is the hang this avoids.
func groupSettled(e *env, plan *models.GroupPlan) (bool, error) {
	if plan.Running {
		return false, nil
	}

	pending, failed := 0, 0
	for _, task := range plan.Tasks {
		switch {
		case !terminalStatus(task.Status):
			pending++
		case task.Status == models.StatusError:
			failed++
		}
	}
	if pending > 0 {
		fmt.Fprintf(e.out, "\ngroup %s is not running — start it with `fanout plan %s --start`\n",
			shortID(plan.GroupID), shortID(plan.GroupID))
		return true, nil
	}
	if failed > 0 {
		fmt.Fprintf(e.out, "\n%s of %d failed\n", plural(failed, "subtask"), len(plan.Tasks))
		return true, errTaskFailed
	}
	fmt.Fprintf(e.out, "\ndone: %s finished\n", plural(len(plan.Tasks), "subtask"))
	return true, nil
}

func terminalStatus(status string) bool {
	switch status {
	case models.StatusDone, models.StatusError, models.StatusStopped:
		return true
	}
	return false
}
