package agent

import (
	"context"
	"fmt"
	"log"
	"strings"

	"fanoutd/internal/models"
	"fanoutd/internal/openrouter"
	"fanoutd/internal/store"
)

// Review is the second opinion between a run ending and its work being filed.
// It is the same loop with the write tools taken away and the finish tool
// replaced by a verdict, which is why it lives beside run() rather than in a
// service of its own: the transcript handling, the sandbox, the tool execution
// and the trace are all things run() already got right.
//
// The whole value is in what the reviewer is not given. It never sees the
// author's trace, only the goal, the criteria settled before any work started,
// the author's summary as a claim to check, and the files. A reviewer replaying
// the author's reasoning inherits the author's reasons for the shortcuts.

// reviewMaxSteps bounds one verdict. A reviewer needs to list, read a few files
// and run something; one that has not reached a verdict in ten steps is reading
// the whole workspace rather than checking the criteria.
const reviewMaxSteps = 10

// maxReviewRounds is how many times one line of work may be sent back before it
// stops going round and waits for a person. Without it a task the model cannot
// fix bounces between todo and review against a metered endpoint for as long as
// the server is up.
const maxReviewRounds = 2

// reviewPrefix marks the trace steps a review pass writes. They are recorded on
// the task so the verdict is visible where the work is, and skipped when that
// task's own transcript is replayed - an author must not be handed a critique of
// itself as though it had written it.
const reviewPrefix = "review: "

const reviewSystemPrompt = `You are reviewing work produced by another agent. You did not write it and you are not here to finish it.

You are given the goal, the acceptance criteria that were settled before any work
started, the summary the author wrote, and the files. You have read_file and
list_files. You cannot change anything, and you should not want to: your job ends
with a verdict.

Work through the criteria one at a time. For each, find the evidence in the files
or by running the thing, and say what the evidence was. A criterion you did not
check is a criterion that failed.

Treat the author's summary as a claim, not as a report. It is the most common place
for work to be described as finished when it is not, and confirming it against the
files is most of what you are here for.

Then call exactly one of:

  pass    — every criterion holds, and you checked each one
  reject  — any criterion does not hold

Reject on the criteria and on things that are plainly broken. Do not reject over
style, structure, naming, or what you would have built instead; the author was not
asked for those and the rework agent will spend a run on whatever you write. When
you reject, say what is wrong, where, and how the rework agent will know it has
fixed it. Findings that do not name a file are findings nobody can act on.

You must end with pass or reject. Reaching neither is not a neutral outcome — it
leaves the work stranded.

If you cannot make tool calls, reply with a single JSON object and nothing else:

  {"tool": {"name":"read_file","path":"index.html"}}
  {"tool": {"name":"reject","findings":"..."}}`

const reviewShellPrompt = `

You also have run_command, which runs a shell command line in the workspace. Use it:
a criterion you verified by running the thing is worth more than one you verified by
reading it, and it is the only way to tell working code from plausible code.

The sandbox has no network access, so anything that downloads dependencies will fail.
That is a fact about this machine and not a fault in the work — never reject over it.

Some deliverables cannot be executed here: a page needs a browser, a UI needs a screen.
Do not spend steps chasing a runtime that is not available. Check those by reading, say
in your verdict which criteria you could not execute, and judge them on the files.`

// SetReview turns the review stage on and names the model that runs it. An empty
// model leaves the reviewer on whatever the task itself used, which is the weak
// configuration and is documented as such - a model reviewing its own output
// agrees with it.
func (l *Loop) SetReview(enabled bool, model string) {
	l.mu.Lock()
	l.review = enabled
	l.reviewModel = strings.TrimSpace(model)
	l.mu.Unlock()
}

func (l *Loop) reviewEnabled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.review
}

// reviewSettings reads both fields under one lock, since a pass needs them
// together.
func (l *Loop) reviewSettings() (bool, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.review, l.reviewModel
}

// reviewTarget is what one verdict covers. Anchor is where the trace and the
// rework hang: for a solo task it is the task, and for a group it is the subtask
// the schedule put last, which is the integration subtask wherever there is one
// and is in every case the only subtask that saw the assembled work.
type reviewTarget struct {
	anchor   models.Task
	covers   []models.Task
	goal     string
	criteria string
	summary  string
}

// reviewAfterRun reviews a solo task whose run has just ended. Subtasks of a
// breakdown are skipped here and reviewed with their group: bouncing one subtask
// on its own would invalidate every sibling that already read its output, and
// nothing tracks a stale read.
func (l *Loop) reviewAfterRun(ctx context.Context, taskID string) {
	if !l.reviewEnabled() || stopped(ctx) {
		return
	}
	task, err := l.store.GetTask(taskID)
	if err != nil || task == nil || task.GroupID != "" {
		return
	}
	if !awaitingReview(*task) {
		return
	}
	l.runReview(ctx, reviewTarget{
		anchor:   *task,
		covers:   []models.Task{*task},
		goal:     task.Goal,
		criteria: task.Criteria,
		summary:  task.Summary,
	})
}

// reviewGroupAfterRun reviews a whole breakdown once its schedule has finished.
// A group is judged as one thing because that is the only level at which the
// question makes sense: the subtasks were split by file, and whether the idea was
// achieved is a property of what they add up to.
//
// A group with any subtask that did not finish is not reviewed. There is nothing
// coherent to hold to the criteria, and the failure is already on the board.
func (l *Loop) reviewGroupAfterRun(ctx context.Context, plan *Plan) {
	if !l.reviewEnabled() || stopped(ctx) {
		return
	}
	tasks, err := l.store.TasksInGroup(plan.GroupID)
	if err != nil || len(tasks) == 0 {
		return
	}
	for _, t := range tasks {
		if !awaitingReview(t) {
			return
		}
	}

	anchor := anchorTask(plan, tasks)
	if anchor == nil {
		return
	}

	idea, criteria, summaries := "", []string{}, []string{}
	for _, t := range tasks {
		if idea == "" {
			idea = GroupIdea(t.Description)
		}
		if c := strings.TrimSpace(t.Criteria); c != "" {
			criteria = append(criteria, c)
		}
		if s := strings.TrimSpace(t.Summary); s != "" {
			summaries = append(summaries, fmt.Sprintf("%s: %s", t.Title, s))
		}
	}
	if idea == "" {
		idea = anchor.Goal
	}

	l.runReview(ctx, reviewTarget{
		anchor:   *anchor,
		covers:   tasks,
		goal:     idea,
		criteria: strings.Join(criteria, "\n"),
		summary:  strings.Join(summaries, "\n\n"),
	})
}

// awaitingReview reports a task parked in the review column by a run that
// produced something. Anything else - failed, stopped, moved by hand - is not
// the reviewer's to judge.
func awaitingReview(t models.Task) bool {
	return t.Column == models.ColumnReview && t.Status == models.StatusDone
}

// anchorTask picks the last subtask of the last wave. The schedule already put
// the subtask that reads everyone else's output there, so this needs no flag of
// its own and stays right if the partition changes.
func anchorTask(plan *Plan, tasks []models.Task) *models.Task {
	if len(plan.Waves) == 0 {
		return nil
	}
	last := plan.Waves[len(plan.Waves)-1]
	if len(last) == 0 {
		return nil
	}
	id := last[len(last)-1]
	for i := range tasks {
		if tasks[i].ID == id {
			return &tasks[i]
		}
	}
	return nil
}

// runReview drives one verdict. It mirrors run(), minus the parts that only make
// sense for an author: there is no repeat guard, because the step cap is tighter
// than the guard would be, and no concede, because a review that produced no
// verdict has produced nothing at all.
func (l *Loop) runReview(ctx context.Context, t reviewTarget) {
	_, model := l.reviewSettings()
	if model == "" {
		model = t.anchor.Model
	}

	ws, err := l.Workspace(t.anchor.ID)
	if err != nil {
		l.reviewFailed(t, 0, fmt.Sprintf("could not open the workspace to review: %v", err))
		return
	}

	l.mu.Lock()
	sandbox := l.sandbox
	l.mu.Unlock()
	// Sandboxed but never Owned: the reviewer holds no claims because it writes
	// nothing, and reviewTool refuses the calls that would.
	ws = ws.Sandboxed(t.anchor.ID, sandbox)

	files, _ := ws.List()
	messages := reviewMessages(t, ws.Root(), files, sandbox != nil)

	prior, err := l.store.ListTraceSteps(t.anchor.ID)
	if err != nil {
		l.reviewFailed(t, 0, err.Error())
		return
	}
	step := len(prior)
	parseFailures := 0

	for i := 0; i < reviewMaxSteps; i++ {
		if stopped(ctx) {
			return
		}

		opts := openrouter.ChatOptions{Tools: ReviewToolDefs(sandbox != nil), Model: model}
		if parseFailures > 0 {
			opts = openrouter.ChatOptions{ForceJSON: true, Model: model}
		}

		resp, err := l.client.Chat(ctx, messages, opts)
		if err != nil {
			if stopped(ctx) {
				return
			}
			l.reviewFailed(t, step, fmt.Sprintf("review model call failed at step %d: %v", step, err))
			return
		}

		step++
		result, err := parseResponse(resp)
		if err != nil {
			parseFailures++
			l.store.AddTraceStep(t.anchor.ID, step, reviewPrefix+actionParseFailure, "", resp.Content, "", parseFailureFeedback(err))
			messages = append(messages,
				openrouter.MsgBlock{Role: "assistant", Content: truncate(resp.Content, 500)},
				openrouter.MsgBlock{Role: "user", Content: parseFailureFeedback(err)},
			)
			if parseFailures >= parseFailureLimit {
				l.reviewFailed(t, step, fmt.Sprintf("the reviewer returned an unusable response %d times in a row", parseFailures))
				return
			}
			continue
		}
		parseFailures = 0

		verdict, note, calls := splitVerdict(result.Tools)

		// Calls run even on the turn that reaches a verdict: a reviewer routinely
		// reads one last file alongside its pass, and the exchange has to be
		// complete whether or not anything follows it.
		exchanges := make([]models.ToolExchange, 0, len(calls))
		for _, pc := range calls {
			out := ""
			if !reviewTool(pc.Call.Name) {
				out = fmt.Sprintf("error: %s is not available to a reviewer. You are checking this work, not changing it. Report what is wrong with %s through reject instead.", pc.Call.Name, describeCall(pc.Call))
			} else if res, err := ws.ExecContext(ctx, pc.Call); err != nil {
				out = "error: " + err.Error()
			} else {
				out = res
			}
			exchanges = append(exchanges, models.ToolExchange{
				ID: pc.ID, Name: pc.Call.Name, Arguments: replayArgs(pc.Args), Result: out,
			})
		}

		entry := store.TraceEntry{
			TaskID: t.anchor.ID, Step: step, Response: result.Text, Calls: exchanges,
			Action: reviewPrefix + loggedAction(result.Action, calls),
		}
		entry.ToolName, entry.ToolResult = summarizeExchanges(exchanges)
		l.store.AddTrace(entry)
		messages = append(messages, replayStep(traceOf(entry), true)...)

		if verdict != "" {
			l.settleReview(ctx, t, step, verdict, note)
			return
		}
	}

	l.reviewFailed(t, step, fmt.Sprintf("the reviewer reached no verdict in %d steps", reviewMaxSteps))
}

// traceOf renders a just-written entry as the row it became, so the reviewer's
// own transcript is built by the same code that replays an author's. Keeping one
// path means a change to how tool exchanges are replayed cannot silently apply to
// only one of them.
func traceOf(e store.TraceEntry) models.TraceStep {
	return models.TraceStep{
		StepNumber: e.Step, Action: e.Action, Response: e.Response,
		ToolName: e.ToolName, ToolResult: e.ToolResult, Calls: e.Calls,
	}
}

// splitVerdict separates a terminal call from the ordinary ones beside it. A
// model that reads a file and passes in the same turn means both, and dropping
// either half would lose a tool result the transcript is expected to carry.
func splitVerdict(tools []pendingCall) (verdict, note string, rest []pendingCall) {
	for _, pc := range tools {
		switch strings.ToLower(strings.TrimSpace(pc.Call.Name)) {
		case passTool:
			if verdict == "" {
				verdict, note = passTool, pc.Call.Summary
			}
		case rejectTool:
			// Reject wins over pass in the same turn. A model that emitted both
			// found something, and filing that as accepted is the one mistake
			// here that nobody sees.
			verdict, note = rejectTool, pc.Call.Findings
			if note == "" {
				note = pc.Call.Summary
			}
		default:
			rest = append(rest, pc)
		}
	}
	return verdict, strings.TrimSpace(note), rest
}

// settleReview files the verdict against every task it covers.
func (l *Loop) settleReview(ctx context.Context, t reviewTarget, step int, verdict, note string) {
	if verdict == passTool {
		if note == "" {
			note = "Reviewed against the criteria; no faults found."
		}
		l.store.AddTraceStep(t.anchor.ID, step+1, reviewPrefix+"passed", "", "", "", note)
		for _, task := range t.covers {
			if err := l.store.SetTaskFinished(task.ID, appendVerdict(task.Summary, "Review passed", note)); err != nil {
				log.Printf("review passed task %s but could not file it: %v\n", shortID(task.ID), err)
			}
		}
		return
	}

	if note == "" {
		note = "Rejected without stating what was wrong."
	}
	l.store.AddTraceStep(t.anchor.ID, step+1, reviewPrefix+"rejected", "", "", "", note)
	for _, task := range t.covers {
		if err := l.store.SetTaskSummary(task.ID, appendVerdict(task.Summary, "Review rejected", note)); err != nil {
			log.Printf("review rejected task %s but could not record it: %v\n", shortID(task.ID), err)
		}
	}

	// The bound. Past it the work stops going round and waits for somebody, which
	// is what `fanout blocked` lists.
	if t.anchor.ReviewRound >= maxReviewRounds {
		reason := fmt.Sprintf("review rejected this work %d times; it needs a person rather than another run", t.anchor.ReviewRound+1)
		for _, task := range t.covers {
			l.store.SetTaskStatus(task.ID, models.StatusError, reason)
		}
		return
	}

	rework, err := l.store.CreateTaskFrom(store.NewTask{
		Title:       reworkTitle(t.anchor.Title, t.anchor.ReviewRound+1),
		Description: reworkContext(t),
		Goal:        note,
		Criteria:    t.criteria,
		ReviewRound: t.anchor.ReviewRound + 1,
		Model:       t.anchor.Model,
		// The shared workspace, but outside the group. A rework pass has to be
		// free to touch whichever file the findings name, and the claims were
		// drawn around a partition that has already been run.
		WorkspaceID: workspaceID(&t.anchor),
		ParentID:    t.anchor.ID,
	})
	if err != nil {
		log.Printf("review rejected %s but could not create the rework task: %v\n", shortID(t.anchor.ID), err)
		return
	}
	if stopped(ctx) {
		return
	}
	if err := l.Start(rework.ID); err != nil {
		log.Printf("created rework task %s but could not start it: %v\n", shortID(rework.ID), err)
	}
}

// reviewFailed records a review that never reached a verdict. The work itself is
// untouched and stays in the review column: it was neither accepted nor faulted,
// and pretending otherwise in either direction would be a verdict this pass did
// not reach. The status is what puts it in front of a person.
func (l *Loop) reviewFailed(t reviewTarget, step int, reason string) {
	l.store.AddTraceStep(t.anchor.ID, step+1, reviewPrefix+"no verdict", "", "", "", reason)
	for _, task := range t.covers {
		l.store.SetTaskStatus(task.ID, models.StatusError, "review did not complete: "+reason)
	}
}

// appendVerdict adds the review's word to what the author said, rather than
// replacing it. Both halves matter afterwards: the summary is what the author
// claimed, and the verdict is what somebody else made of it.
func appendVerdict(summary, label, note string) string {
	summary = strings.TrimSpace(summary)
	entry := fmt.Sprintf("%s: %s", label, strings.TrimSpace(note))
	if summary == "" {
		return entry
	}
	return summary + "\n\n" + entry
}

// reworkTitle labels the fix pass with the round it belongs to, replacing any
// suffix from the round before instead of stacking them.
func reworkTitle(title string, round int) string {
	base := strings.TrimSpace(title)
	if i := strings.LastIndex(base, " — rework "); i > 0 {
		base = base[:i]
	}
	return fmt.Sprintf("%s — rework %d", base, round)
}

// reworkContext is the rework agent's "Details:". The findings are its goal, so
// this says only what the findings cannot: where they came from, and that the
// work is already in the workspace and is meant to be repaired rather than
// replaced.
func reworkContext(t reviewTarget) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This is a rework pass. The work is already in your workspace and was reviewed by another agent, which sent it back. Your goal above is what that review found.\n\n")
	fmt.Fprintf(&b, "The original goal was: %s\n\n", truncate(strings.TrimSpace(t.goal), 400))
	b.WriteString("Fix what the findings name and nothing else. Read the files before you change them — most of the work is right, and rewriting it from scratch loses the parts that already passed. Do not add features, and do not start a new version alongside the old one.")
	return b.String()
}

// reviewMessages is the reviewer's opening prompt. It is built from the
// workspace and the criteria and never from the author's trace, which is the
// whole point of running a second agent.
func reviewMessages(t reviewTarget, workspace string, files []FileEntry, sandboxed bool) []openrouter.MsgBlock {
	var b strings.Builder
	fmt.Fprintf(&b, "Goal the work was given: %s\n", strings.TrimSpace(t.goal))
	fmt.Fprintf(&b, "Workspace directory: %s\n", workspace)

	b.WriteString("\nAcceptance criteria:\n")
	if c := criteriaList(t.criteria); c != "" {
		b.WriteString(c)
	} else {
		// A task created by hand carries no criteria. Saying so is better than
		// letting the reviewer infer a standard of its own and reject against it.
		b.WriteString("  (none were recorded for this task, so hold it to the goal above and to nothing more)\n")
	}

	if s := strings.TrimSpace(t.summary); s != "" {
		fmt.Fprintf(&b, "\nWhat the author claims it produced:\n%s\n", truncate(s, 2000))
	}

	if len(files) > 0 {
		b.WriteString("\nFiles in the workspace:\n")
		for _, f := range files {
			fmt.Fprintf(&b, "  %s (%d bytes)\n", f.Path, f.Size)
		}
	} else {
		b.WriteString("\nThe workspace is empty. Nothing was produced.\n")
	}

	b.WriteString("\nCheck the criteria against the files. Then pass or reject.")

	system := reviewSystemPrompt
	if sandboxed {
		system += reviewShellPrompt
	}
	return []openrouter.MsgBlock{
		{Role: "system", Content: system},
		{Role: "user", Content: b.String()},
	}
}

// criteriaList renders stored criteria as the list both the author and the
// reviewer are shown. Rendering it in one place is what keeps the two from
// drifting into different wordings of the same requirement.
func criteriaList(criteria string) string {
	var b strings.Builder
	for _, line := range strings.Split(criteria, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			fmt.Fprintf(&b, "  - %s\n", line)
		}
	}
	return b.String()
}
