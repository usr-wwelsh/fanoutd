package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"fanoutd/internal/models"
)

// watchInterval is the poll period. The agent sleeps 500ms between steps and a
// model call takes seconds, so anything faster is just load on the server.
const watchInterval = 2 * time.Second

func cmdWatch(e *env, args []string) error {
	fs := e.flags("watch")
	interval := fs.Duration("interval", watchInterval, "poll period")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fanout watch <id> [--interval 2s]")
		fs.PrintDefaults()
	}
	id, err := e.resolve(fs, args)
	if err != nil {
		return e.describeErr(err)
	}
	return watchTaskEvery(e, id, *interval)
}

func watchTask(e *env, id string) error { return watchTaskEvery(e, id, watchInterval) }

// watchTaskEvery prints steps as they land and returns when the task reaches a
// terminal state. The exit code is what makes this composable: a task that ends
// in error fails the command, so `fanout start x --watch && deploy` behaves.
func watchTaskEvery(e *env, id string, interval time.Duration) error {
	printed := 0
	for {
		state, err := e.client.Status(e.ctx, id)
		if err != nil {
			return e.describeErr(err)
		}
		steps, err := e.client.Trace(e.ctx, id)
		if err != nil {
			return e.describeErr(err)
		}
		for _, s := range steps[min(printed, len(steps)):] {
			fmt.Fprintln(e.out, stepLine(s))
		}
		printed = len(steps)

		if done, err := terminal(e, state); done {
			return err
		}

		select {
		case <-e.ctx.Done():
			fmt.Fprintf(os.Stderr, "\nstopped watching; %s is still running on %s\n", shortID(id), e.cfg.server)
			return context.Canceled
		case <-time.After(interval):
		}
	}
}

// terminal reports whether the run has settled, and with what outcome.
func terminal(e *env, state *runState) (bool, error) {
	if state.Running {
		return false, nil
	}
	task := state.Task
	switch task.Status {
	case models.StatusDone:
		if task.Summary != "" {
			fmt.Fprintf(e.out, "\ndone: %s\n", task.Summary)
		} else {
			fmt.Fprintln(e.out, "\ndone")
		}
		return true, nil
	case models.StatusError:
		fmt.Fprintf(e.out, "\nerror: %s\n", task.Error)
		return true, errTaskFailed
	case models.StatusStopped:
		fmt.Fprintln(e.out, "\nstopped")
		return true, nil
	case models.StatusIdle:
		// Nothing to follow. Watching an idle task would otherwise poll
		// forever waiting for a run nobody started.
		fmt.Fprintf(e.out, "%s is idle — start it with `fanout start %s`\n", shortID(task.ID), shortID(task.ID))
		return true, nil
	case models.StatusRunning:
		// Status says running but no loop is attached: the server restarted
		// mid-run. This never resolves on its own, so do not wait for it.
		fmt.Fprintf(e.out, "\nrun is no longer attached (server restarted?) — resume with `fanout start %s`\n", shortID(task.ID))
		return true, errTaskFailed
	}
	return false, nil
}
