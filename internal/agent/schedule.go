package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"fanoutd/internal/models"
)

// deriveDeps builds the dependency edges of a breakdown from its file
// partition: a task that reads a path depends on the task that writes it.
//
// The edges are derived rather than declared because a model asked for a
// dependency list produces something plausible and wrong, while the same model
// asked which files a subtask reads and writes is answering about the work
// itself. "Write the tests" and "write the thing they test" order themselves as
// long as the file lists are right, and if they are wrong the schedule is wrong
// in the same direction as the claims — one thing to get right, not two.
//
// reads maps a task to the paths it consumes, writers maps a path to its owner.
// Paths nobody in the group writes, and a task reading what it writes itself,
// produce no edge.
func deriveDeps(taskIDs []string, reads map[string][]string, writers map[string]string) map[string][]string {
	inGroup := make(map[string]bool, len(taskIDs))
	for _, id := range taskIDs {
		inGroup[id] = true
	}

	deps := make(map[string][]string, len(taskIDs))
	for _, id := range taskIDs {
		seen := map[string]bool{}
		for _, path := range reads[id] {
			owner, written := writers[path]
			if !written || owner == id || !inGroup[owner] || seen[owner] {
				continue
			}
			seen[owner] = true
			deps[id] = append(deps[id], owner)
		}
		sort.Strings(deps[id])
	}
	return deps
}

// topoWaves groups tasks so that everything in one wave can run at once and
// every wave depends only on those before it. It is Kahn's algorithm, kept
// separate from execution because it is also the plan a user can be shown
// before anything runs, and because a cycle must be caught at plan time.
//
// Execution does not follow the waves in lockstep — see runGroup, which starts
// a task the moment its own dependencies are done rather than waiting for the
// rest of its wave. Model latency varies too much for lockstep to be worth it.
func topoWaves(taskIDs []string, deps map[string][]string) ([][]string, error) {
	remaining := make(map[string]bool, len(taskIDs))
	for _, id := range taskIDs {
		remaining[id] = true
	}

	waves := [][]string{}
	for len(remaining) > 0 {
		wave := []string{}
		for _, id := range taskIDs {
			if !remaining[id] {
				continue
			}
			ready := true
			for _, dep := range deps[id] {
				if remaining[dep] {
					ready = false
					break
				}
			}
			if ready {
				wave = append(wave, id)
			}
		}
		if len(wave) == 0 {
			return nil, &cycleError{IDs: stillRemaining(taskIDs, remaining)}
		}
		for _, id := range wave {
			delete(remaining, id)
		}
		waves = append(waves, wave)
	}
	return waves, nil
}

// cycleError names the tasks left in a graph that cannot be ordered. It carries
// them rather than only a message so a breakdown being validated before it is
// created can report the titles the user typed instead of ids they have never
// seen.
type cycleError struct{ IDs []string }

func (e *cycleError) Error() string {
	return fmt.Sprintf("dependency cycle among %s", shortIDs(e.IDs))
}

func stillRemaining(taskIDs []string, remaining map[string]bool) []string {
	out := []string{}
	for _, id := range taskIDs {
		if remaining[id] {
			out = append(out, id)
		}
	}
	return out
}

func shortIDs(ids []string) string {
	short := make([]string, 0, len(ids))
	for _, id := range ids {
		short = append(short, shortID(id))
	}
	return strings.Join(short, ", ")
}

// Plan is a breakdown's schedule, resolved but not started.
//
// Writes and Reads are the partition Deps was derived from, keyed by task. They
// are carried alongside the edges because an edge on its own says only that one
// task waits for another, while the path that produced it says why.
type Plan struct {
	GroupID string              `json:"group_id"`
	Deps    map[string][]string `json:"deps"`
	Waves   [][]string          `json:"waves"`
	Writes  map[string][]string `json:"writes"`
	Reads   map[string][]string `json:"reads"`
}

// PlanGroup resolves the dependency graph for a breakdown without running
// anything, so a cycle or a bad partition surfaces before the first token is
// spent.
func (l *Loop) PlanGroup(groupID string) (*Plan, error) {
	tasks, err := l.store.TasksInGroup(groupID)
	if err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("no tasks in group %s: %w", shortID(groupID), ErrGroupNotFound)
	}

	// Subtasks of one breakdown share a workspace by construction; claims are
	// scoped to it, so a group spanning two would silently lose its edges.
	workspace := tasks[0].WorkspaceID
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t.WorkspaceID != workspace {
			return nil, fmt.Errorf("group %s spans more than one workspace", shortID(groupID))
		}
		ids = append(ids, t.ID)
	}

	reads, err := l.store.GroupReads(workspace)
	if err != nil {
		return nil, err
	}
	writers, err := l.store.Writers(workspace)
	if err != nil {
		return nil, err
	}

	deps := deriveDeps(ids, reads, writers)
	waves, err := topoWaves(ids, deps)
	if err != nil {
		return nil, err
	}
	return &Plan{
		GroupID: groupID,
		Deps:    deps,
		Waves:   waves,
		Writes:  writesByTask(writers, ids),
		Reads:   reads,
	}, nil
}

// writesByTask inverts the path-to-owner map into the owner-to-paths one a
// caller rendering a task wants, dropping paths owned outside the group.
func writesByTask(writers map[string]string, ids []string) map[string][]string {
	inGroup := make(map[string]bool, len(ids))
	for _, id := range ids {
		inGroup[id] = true
	}
	out := map[string][]string{}
	for path, owner := range writers {
		if inGroup[owner] {
			out[owner] = append(out[owner], path)
		}
	}
	for _, paths := range out {
		sort.Strings(paths)
	}
	return out
}

// StartGroup runs a breakdown's subtasks in dependency order, several at once,
// and returns once the schedule is under way. Failures are recorded on the
// tasks, as with a single run.
func (l *Loop) StartGroup(groupID string) error {
	plan, err := l.PlanGroup(groupID)
	if err != nil {
		return err
	}

	ctx, ok := l.claimGroup(context.Background(), groupID)
	if !ok {
		return ErrGroupRunning
	}

	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer l.clearGroup(groupID)
		l.runGroup(ctx, plan)
		// Once the whole schedule has settled, not per subtask: sending one
		// subtask back would invalidate every sibling that already read its
		// output, and the claims arbitrate concurrent writes rather than stale
		// reads.
		l.reviewGroupAfterRun(ctx, plan)
	}()
	return nil
}

// GroupView is the plan plus the state of every subtask, which is what a caller
// displaying a breakdown needs and the only thing the HTTP layer serves.
//
// The plan is re-derived from the claims on every call rather than stored, so a
// subtask that claimed a path the breakdown never predicted shows up here as a
// new edge. That is the honest picture: the running schedule was fixed when it
// started, but the file partition is what the group actually is.
func (l *Loop) GroupView(groupID string) (*models.GroupPlan, error) {
	plan, err := l.PlanGroup(groupID)
	if err != nil {
		return nil, err
	}
	tasks, err := l.store.TasksInGroup(groupID)
	if err != nil {
		return nil, err
	}
	// Every subtask carries the same idea, so the first one that still has it
	// answers for the group.
	idea := ""
	for _, t := range tasks {
		if idea = GroupIdea(t.Description); idea != "" {
			break
		}
	}
	return &models.GroupPlan{
		GroupID: groupID,
		Idea:    idea,
		Waves:   plan.Waves,
		Deps:    plan.Deps,
		Writes:  plan.Writes,
		Reads:   plan.Reads,
		Tasks:   tasks,
		Running: l.IsGroupRunning(groupID),
	}, nil
}

// claimGroup registers a breakdown as busy and returns the context its work runs
// under, or false if something already holds it. A review takes the claim as
// well as a schedule does: a verdict is work being done to the whole group, and
// a start arriving underneath it would re-run the subtasks the reviewer is in
// the middle of reading.
func (l *Loop) claimGroup(parent context.Context, groupID string) (context.Context, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, busy := l.groups[groupID]; busy {
		return nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	l.groups[groupID] = cancel
	return ctx, true
}

func (l *Loop) IsGroupRunning(groupID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, running := l.groups[groupID]
	return running
}

// StopGroup cancels a running schedule and every task under it.
func (l *Loop) StopGroup(groupID string) bool {
	l.mu.Lock()
	cancel, running := l.groups[groupID]
	l.mu.Unlock()
	if !running {
		return false
	}
	cancel()
	return true
}

func (l *Loop) clearGroup(groupID string) {
	l.mu.Lock()
	if cancel, ok := l.groups[groupID]; ok {
		cancel()
		delete(l.groups, groupID)
	}
	l.mu.Unlock()
}

// runGroup is the rolling scheduler. A task starts as soon as its own
// dependencies have finished and a parallel slot is free, rather than waiting
// for the rest of its topological wave — with model latency varying by an order
// of magnitude between subtasks, lockstep waves would leave slots idle behind
// the slowest sibling in each one.
func (l *Loop) runGroup(ctx context.Context, plan *Plan) {
	// A task is "settled" once it will never run again, whether it finished,
	// failed, or was skipped. Dependents key off outcome, not settlement.
	succeeded := map[string]bool{}
	failed := map[string]bool{}
	running := map[string]<-chan struct{}{}

	// Resuming a schedule runs what is left of it, not the whole thing again.
	// A halted breakdown is ordinarily restarted from the board, and starting it
	// re-ran subtasks that had already filed their work: the same files written
	// twice, the same tokens spent, and the anchor reaching goal-met again on a
	// step it had already reached. Work already filed counts as succeeded, which
	// is what its dependents were waiting for anyway.
	pending := map[string]bool{}
	for _, wave := range plan.Waves {
		for _, id := range wave {
			if l.taskFiled(id) {
				succeeded[id] = true
				continue
			}
			pending[id] = true
		}
	}

	for len(pending) > 0 || len(running) > 0 {
		if ctx.Err() != nil {
			l.abandon(pending, "group stopped before this subtask started")
			return
		}

		// Skip anything whose upstream will never produce what it reads. Running
		// it anyway means an agent staring at a file that is not there.
		//
		// The sweep follows the waves rather than ranging over the pending map,
		// because a skip is itself a failure its own dependents must inherit:
		// visiting a task before the blocker two levels up has been marked
		// would strand it, pending forever behind something that will never run.
		for _, wave := range plan.Waves {
			for _, id := range wave {
				if !pending[id] {
					continue
				}
				if blocker, blocked := firstBlocked(plan.Deps[id], failed); blocked {
					delete(pending, id)
					failed[id] = true
					l.blockTask(id, blocker)
				}
			}
		}

		started := false
		limit := l.parallelLimit()
		for _, id := range l.readyOrder(plan, pending, succeeded) {
			if len(running) >= limit {
				break
			}
			done, err := l.startTracked(ctx, id)
			if err != nil {
				delete(pending, id)
				failed[id] = true
				l.fail(id, 0, fmt.Sprintf("could not start subtask: %v", err))
				continue
			}
			delete(pending, id)
			running[id] = done
			started = true
		}

		if len(running) == 0 {
			if !started && len(pending) > 0 {
				// Nothing running, nothing startable, work left: the graph said
				// otherwise, so fail loudly rather than spin.
				l.abandon(pending, "subtask was never scheduled — its dependencies never settled")
			}
			if len(pending) == 0 {
				return
			}
			continue
		}

		id := l.awaitOne(ctx, running)
		if id == "" {
			l.abandon(pending, "group stopped before this subtask started")
			return
		}
		delete(running, id)
		if l.taskSucceeded(id) {
			succeeded[id] = true
		} else {
			failed[id] = true
		}
	}
}

// readyOrder lists the pending tasks whose dependencies have all succeeded, in
// the group's stable order so a schedule is reproducible.
func (l *Loop) readyOrder(plan *Plan, pending map[string]bool, succeeded map[string]bool) []string {
	ready := []string{}
	for _, wave := range plan.Waves {
		for _, id := range wave {
			if !pending[id] {
				continue
			}
			ok := true
			for _, dep := range plan.Deps[id] {
				if !succeeded[dep] {
					ok = false
					break
				}
			}
			if ok {
				ready = append(ready, id)
			}
		}
	}
	return ready
}

// awaitOne blocks until one running subtask ends, returning its id, or "" if
// the group was cancelled first.
func (l *Loop) awaitOne(ctx context.Context, running map[string]<-chan struct{}) string {
	type result struct{ id string }
	out := make(chan result, len(running))
	stop := make(chan struct{})
	defer close(stop)

	for id, done := range running {
		go func(id string, done <-chan struct{}) {
			select {
			case <-done:
				select {
				case out <- result{id}:
				case <-stop:
				}
			case <-stop:
			}
		}(id, done)
	}

	select {
	case r := <-out:
		return r.id
	case <-ctx.Done():
		return ""
	}
}

func firstBlocked(deps []string, failed map[string]bool) (string, bool) {
	for _, dep := range deps {
		if failed[dep] {
			return dep, true
		}
	}
	return "", false
}

// taskSucceeded reads the outcome the run recorded for itself. A conceded run
// counts: it produced files its dependents can read.
func (l *Loop) taskSucceeded(taskID string) bool {
	task, err := l.store.GetTask(taskID)
	if err != nil || task == nil {
		return false
	}
	return task.Status == models.StatusDone
}

// taskFiled reports a subtask whose work is done and put away — awaiting a
// verdict, or already accepted. It is deliberately narrower than "done": a
// finished run that somebody dragged back to To-Do was dragged there to be run
// again, and the column is the only place that intent is recorded.
func (l *Loop) taskFiled(taskID string) bool {
	task, err := l.store.GetTask(taskID)
	if err != nil || task == nil || task.Status != models.StatusDone {
		return false
	}
	return task.Column == models.ColumnReview || task.Column == models.ColumnFinished
}

func (l *Loop) blockTask(taskID, blocker string) {
	msg := fmt.Sprintf("skipped: depends on subtask %s, which did not finish", shortID(blocker))
	l.store.AddTraceStep(taskID, 0, "subtask blocked", "", "", "", msg)
	l.store.SetTaskStatus(taskID, models.StatusError, msg)
}

func (l *Loop) abandon(pending map[string]bool, reason string) {
	for id := range pending {
		l.store.SetTaskStatus(id, models.StatusStopped, reason)
		delete(pending, id)
	}
}
