package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"fanoutd/internal/llm"
	"fanoutd/internal/models"
	"fanoutd/internal/store"
)

// A run is aborted once the same action or tool call has been issued this many times.
const repeatLimit = 3

// A run is aborted after this many consecutive unparseable model responses.
const parseFailureLimit = 3

// actionParseFailure marks a trace step whose model reply could not be read.
const actionParseFailure = "unparseable model response"

// toolResultBudget caps a replayed tool result. It must exceed readPageBytes so
// a full page and its continuation notice reach the model unclipped.
const toolResultBudget = 8000

// toolCallBudget caps the arguments of a replayed call. Unlike a result, these
// are bytes the model wrote and can write again, so the cap is there to keep one
// large file from crowding out the rest of the transcript.
const toolCallBudget = 8000

// transcriptBudget caps the whole replayed trace. The per-item caps above bound
// one step; nothing bounded the sum, so a run's prompt grew by a step every step
// and its token cost grew with the square of its length. Real runs reached 160KB
// of transcript on their final step, and a continued task replays its earlier
// runs too.
//
// Older steps are condensed rather than dropped, since dropping one would strip
// an assistant turn whose tool results are still in the transcript. What is
// elided is recoverable: the files are on disk and read_file is a call away.
const transcriptBudget = 48000

// minFullSteps is how many of the newest steps replay whole whatever the budget
// says. Condensing the step just taken would hide the result the model is meant
// to be reacting to, which is the one thing the transcript is for.
const minFullSteps = 3

// digestBytes is how much of a condensed value survives. Enough to recognise
// what the step did — the path written, the head of a build log — and not enough
// to work from, which is the point: work from the file.
const digestBytes = 240

var ErrAlreadyRunning = errors.New("agent loop already running for this task")

var ErrGroupRunning = errors.New("this breakdown is already running")

// ErrGroupNotFound is a group id with no subtasks behind it. Groups have no
// table of their own, so this is the only way one can be missing.
var ErrGroupNotFound = errors.New("no such group")

// defaultMaxParallel caps how many subtasks of one breakdown run at once. The
// binding constraint is the provider, not the machine: a dozen concurrent
// agents on one OpenRouter key earn rate limits rather than throughput.
const defaultMaxParallel = 3

// defaultMaxSteps bounds one run. It is a budget, not a safety rail: the agent
// stops itself on repetition long before this, so the limit only ever bites a
// run that is still making progress. Set it too low and a task that was three
// steps from a clean build concedes with work that does not compile, and the
// subtasks waiting on it build against the wreckage.
//
// It was 20, which every subtask of a five-part Go breakdown hit while still
// in its build-fix loop. At 40 the same idea on the same model built clean and
// passed its tests. A model that converges quickly never reaches either number,
// so the cost of the higher default falls only on runs that were going to
// concede anyway.
const defaultMaxSteps = 40

const systemPrompt = `You are an autonomous task agent working toward a goal.

You have a private workspace directory and a set of file tools: write_file, read_file,
edit_file, delete_file, list_files, and finish. Call them as tools. Paths are relative
to your workspace.

Any real output the goal asks for must be written to a file with write_file — text in
your response is not saved as a deliverable. Call finish once the goal is fully achieved.

If you cannot make tool calls, reply with a single JSON object and nothing else — no
markdown, no code fences, no XML:

  {"goal_met": false, "next_action": "what you are doing and why", "tool": {"name":"write_file","path":"report.md","content":"..."}}
  {"goal_met": true, "summary": "what you produced, including the files you wrote"}

File contents belong in the "content" string, JSON-escaped. To think without touching
files, omit "tool".

Never repeat an action you have already taken - the result is already in the conversation.
Each step must make new progress.

Whatever you write has to work where it lands, with no build step and nothing installed
first. Nobody bundles it, serves it, or runs a package manager over it afterwards - a
file that needs any of that is a file that does not run.

For anything a browser opens this is the usual way a deliverable dies, and it dies
silently: a page loaded from disk resolves its own imports, so "import * as X from 'lib'"
resolves to nothing and the script stops on its first line with a blank window and no
trace of why. Give every bare specifier a real URL, through an import map in the page or
by importing the URL directly, or write plain scripts with no module imports at all. The
page must work opened straight from the filesystem.`

// shellPrompt is appended only when a sandbox exists. It is deliberately silent
// about which languages are available: the toolchain is whatever the host has,
// and naming a few would narrow what the model tries.
const shellPrompt = `

You also have run_command, which runs a shell command line in your workspace. Use it to
build, test and check the work you have written - a deliverable you have actually
compiled or run is worth more than one you have only written.

Every command already starts in your workspace directory, which already exists. Do not
cd into it, and never mkdir it - use relative paths like "go build ./..." and let the
working directory be what it is.

The sandbox has no network access, so anything that downloads dependencies will fail.
This decides the shape of what you write, not just how you test it: a deliverable that
needs a package installed before it will run is one you cannot run here. Depend on what
is already on the machine, or vendor what you need into the workspace yourself.

Only your workspace and /build are writable. Compiler and package caches already go
outside the workspace, but a binary lands wherever you tell it to: build to /build, as in
"go build -o /build/tool .", so your workspace keeps the source you were asked for rather
than a megabyte of compiled output.

Long-running commands are killed, so prefer targeted builds and tests over whole-repo
ones. Re-running a command you have already run, unchanged, is not progress.`

type Loop struct {
	store   *store.Store
	client  llm.API
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	// groups tracks running breakdown schedules, so one can be stopped as a
	// unit without hunting down its subtasks.
	groups      map[string]context.CancelFunc
	wg          sync.WaitGroup
	maxSteps    int
	maxParallel int
	outputDir   string
	// sandbox is nil when bubblewrap is unavailable or shell commands are
	// switched off, which withholds run_command rather than degrading it.
	sandbox *Sandbox
	// review sends a settled run to a second agent before it is filed, and
	// reviewModel names what that agent runs on. See review.go.
	review      bool
	reviewModel string
	// orchestratorModel names what the breakdown call plans on. Empty leaves it
	// on whatever model the breakdown request itself named. See breakdown.go.
	orchestratorModel string
	// sweep cancels the startup review sweep, which belongs to no task and no
	// group and so is reachable through neither map.
	sweep context.CancelFunc
}

// SetClient points the loop at a different provider. A run already in flight
// picks it up at its next step rather than being cancelled: the transcript is
// the provider-neutral part of a run, so continuing it on a new endpoint is
// exactly what changing the setting asked for.
func (l *Loop) SetClient(c llm.API) {
	l.mu.Lock()
	l.client = c
	l.mu.Unlock()
}

// api reads the current provider. Every call goes through it rather than
// touching l.client, so a swap mid-run is a lock rather than a data race.
func (l *Loop) api() llm.API {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.client
}

// SetSandbox enables the shell tool. A nil sandbox leaves agents file-only.
func (l *Loop) SetSandbox(sb *Sandbox) {
	l.mu.Lock()
	l.sandbox = sb
	l.mu.Unlock()
}

// Sandboxed reports whether agents actually have a shell, which is not the same
// question as whether one was asked for: a sandbox that would not start leaves
// the tool unoffered and the setting on.
func (l *Loop) Sandboxed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sandbox != nil
}

// SetOrchestratorModel names the model the breakdown call plans on. An empty
// model leaves it on whatever the breakdown request itself named — the same
// fallback shape as SetReview, for the same reason: this is a global default,
// not the only way to set it.
func (l *Loop) SetOrchestratorModel(model string) {
	l.mu.Lock()
	l.orchestratorModel = strings.TrimSpace(model)
	l.mu.Unlock()
}

func (l *Loop) orchestratorModelSetting() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.orchestratorModel
}

func NewLoop(s *store.Store, c llm.API, outputDir string) *Loop {
	return &Loop{
		store:       s,
		client:      c,
		cancels:     make(map[string]context.CancelFunc),
		groups:      make(map[string]context.CancelFunc),
		maxSteps:    defaultMaxSteps,
		maxParallel: defaultMaxParallel,
		outputDir:   outputDir,
	}
}

// SetMaxSteps bounds how many steps one run gets before it concedes. Anything
// below one restores the default, which is what an unset config means and also
// what clearing the field on the settings page has to mean — a limit that could
// only ever be raised would leave the last number typed in force with nothing
// on screen saying so.
func (l *Loop) SetMaxSteps(n int) {
	if n < 1 {
		n = defaultMaxSteps
	}
	l.mu.Lock()
	l.maxSteps = n
	l.mu.Unlock()
}

// SetMaxParallel bounds concurrent subtasks within one breakdown. Anything below
// one restores the default, for the same reason.
func (l *Loop) SetMaxParallel(n int) {
	if n < 1 {
		n = defaultMaxParallel
	}
	l.mu.Lock()
	l.maxParallel = n
	l.mu.Unlock()
}

// Start launches the loop in the background. It returns an error only if the
// task cannot be started; failures during the run are recorded on the task.
func (l *Loop) Start(taskID string) error {
	_, err := l.startTracked(context.Background(), taskID)
	return err
}

// startTracked is Start with the two things a scheduler needs: a parent context,
// so cancelling a breakdown cancels its subtasks, and a channel closed when the
// run ends, so the next task can be released without polling the database.
func (l *Loop) startTracked(parent context.Context, taskID string) (<-chan struct{}, error) {
	task, err := l.store.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task %s not found", taskID)
	}

	ctx, ok := l.claimRun(parent, taskID)
	if !ok {
		return nil, ErrAlreadyRunning
	}

	// Starting a task is an instruction to keep working on it, so it withdraws
	// any standing "finished" mark. The loop treats that flag as a live stop
	// signal and re-reads it every step, which is right for a task marked done
	// on the board mid-run but made resuming impossible: a run that conceded
	// with files was filed as finished, so every restart returned on step one
	// and left the task running until the next server restart reclaimed it.
	if err := l.store.ClearFinishFlag(taskID); err != nil {
		l.clearRun(taskID)
		return nil, err
	}
	if err := l.store.SetTaskColumn(taskID, models.ColumnTodo); err != nil {
		l.clearRun(taskID)
		return nil, err
	}
	if err := l.store.SetTaskStatus(taskID, models.StatusRunning, ""); err != nil {
		l.clearRun(taskID)
		return nil, err
	}

	done := make(chan struct{})
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer close(done)
		defer l.clearRun(taskID)
		l.run(ctx, taskID)
		// In the body rather than deferred, so the task stays registered as
		// running while it is reviewed: a verdict is still work being done to it,
		// and starting it again underneath the reviewer would have two agents in
		// one workspace. A subtask returns from here immediately — its group is
		// reviewed whole, once the schedule ends.
		l.reviewAfterRun(ctx, taskID)
	}()
	return done, nil
}

// claimRun registers a task as busy and returns the context that work runs
// under, or false if something already holds it. Review takes a claim as well as
// a run does: a verdict is still work being done to a workspace, and a sweep
// reviewing a task while the user presses start on it would have two agents in
// one directory.
func (l *Loop) claimRun(parent context.Context, taskID string) (context.Context, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, busy := l.cancels[taskID]; busy {
		return nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	l.cancels[taskID] = cancel
	return ctx, true
}

// Stop cancels a running loop. It reports whether a run was actually cancelled.
func (l *Loop) Stop(taskID string) bool {
	l.mu.Lock()
	cancel, running := l.cancels[taskID]
	l.mu.Unlock()
	if !running {
		return false
	}
	cancel()
	return true
}

// StopAll cancels every active run and schedule, for shutdown.
func (l *Loop) StopAll() {
	l.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(l.cancels)+len(l.groups)+1)
	if l.sweep != nil {
		cancels = append(cancels, l.sweep)
	}
	for _, cancel := range l.groups {
		cancels = append(cancels, cancel)
	}
	for _, cancel := range l.cancels {
		cancels = append(cancels, cancel)
	}
	l.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// parallelLimit reads the cap under the lock, since a schedule consults it on
// every pass while SetMaxParallel can be called from an HTTP handler.
func (l *Loop) parallelLimit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.maxParallel
}

// stepLimit reads the step budget under the lock, for the same reason: a run
// consults it on every step, and runs already in flight share the Loop with
// whoever sets it.
func (l *Loop) stepLimit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.maxSteps
}

// Shutdown cancels every active run and waits for the goroutines to record
// their final status, giving up at ctx's deadline. Cancelling alone is not
// enough: a run writes "stopped" from its own goroutine, so exiting straight
// after StopAll leaves the task marked running in the database with nothing
// left to correct it. It reports whether every run finished in time; false
// means ReclaimRunningTasks will clean up the remainder at the next start.
func (l *Loop) Shutdown(ctx context.Context) bool {
	l.StopAll()

	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// DiscardBuildDir drops the scratch a task's shell commands built up. Deleting a
// task removes its workspace, but the build directory sits outside it and lived
// on: a board that had run 62 tasks was holding build directories for 23 that no
// longer existed.
//
// It is not an error for a server without a sandbox, which never made one.
func (l *Loop) DiscardBuildDir(taskID string) error {
	l.mu.Lock()
	sandbox := l.sandbox
	l.mu.Unlock()
	if sandbox == nil {
		return nil
	}
	return sandbox.DiscardTask(taskID)
}

// Workspace returns the sandbox backing a task. Tasks that continue an earlier
// run share one, so this resolves through the task's workspace ID.
func (l *Loop) Workspace(taskID string) (*Workspace, error) {
	task, err := l.store.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task %s not found", taskID)
	}
	return NewWorkspace(l.outputDir, workspaceID(task))
}

// WorkspaceFiles lists the files in a task's workspace, each marked with whether
// this task is the one that wrote it.
func (l *Loop) WorkspaceFiles(taskID string) ([]FileEntry, error) {
	ws, err := l.Workspace(taskID)
	if err != nil {
		return nil, err
	}
	files, err := ws.List()
	if err != nil {
		return nil, err
	}
	return l.markOwned(taskID, files)
}

// markOwned records which of a listing's files the task wrote. Subtasks of one
// breakdown share a workspace, so the raw listing credits every sibling's output
// to whoever asks — five subtasks each reporting the same seven files, none of
// which say which two were theirs. Ownership is only recorded for grouped tasks,
// and a solo task has the workspace to itself, so for those everything in it is
// its own.
func (l *Loop) markOwned(taskID string, files []FileEntry) ([]FileEntry, error) {
	task, err := l.store.GetTask(taskID)
	if err != nil {
		return nil, err
	}

	mine := map[string]bool{}
	if task != nil && task.GroupID != "" {
		owned, err := l.store.OwnedPaths(workspaceID(task), taskID)
		if err != nil {
			return nil, err
		}
		for _, p := range owned {
			mine[p] = true
		}
	}

	for i, f := range files {
		if task == nil || task.GroupID == "" {
			files[i].Owned = true
			continue
		}
		key, ok := normalizeClaimPath(f.Path)
		files[i].Owned = ok && mine[key]
	}
	return files, nil
}

// producedFiles narrows a workspace listing to the files this task itself wrote.
// It is what separates conceding to done from conceding to error: a subtask that
// looped without writing anything must not be credited with its siblings' work.
func (l *Loop) producedFiles(taskID string) ([]FileEntry, error) {
	files, err := l.WorkspaceFiles(taskID)
	if err != nil {
		return nil, err
	}
	var out []FileEntry
	for _, f := range files {
		if f.Owned {
			out = append(out, f)
		}
	}
	return out, nil
}

// workspaceID falls back to the task ID for rows written before workspaces
// could be shared.
func workspaceID(task *models.Task) string {
	if task.WorkspaceID != "" {
		return task.WorkspaceID
	}
	return task.ID
}

func (l *Loop) IsRunning(taskID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, running := l.cancels[taskID]
	return running
}

func (l *Loop) clearRun(taskID string) {
	l.mu.Lock()
	if cancel, ok := l.cancels[taskID]; ok {
		cancel()
		delete(l.cancels, taskID)
	}
	l.mu.Unlock()
}

func (l *Loop) run(ctx context.Context, taskID string) {
	ws, err := l.Workspace(taskID)
	if err == nil {
		err = os.MkdirAll(ws.Root(), 0o755)
	}
	if err != nil {
		l.fail(taskID, 0, fmt.Sprintf("could not create workspace: %v", err))
		return
	}

	// Subtasks of one breakdown share a workspace and can be running at the same
	// moment, so their writes are arbitrated. A task that merely continues an
	// earlier run is alone in its workspace and must stay free to edit anything
	// it inherited — hence the group, not the shared workspace, as the trigger.
	l.mu.Lock()
	sandbox := l.sandbox
	l.mu.Unlock()
	ws = ws.Sandboxed(taskID, sandbox)

	if t, err := l.store.GetTask(taskID); err == nil && t != nil && t.GroupID != "" {
		ws = ws.Owned(taskID, l.store)
	}

	prior, err := l.store.ListTraceSteps(taskID)
	if err != nil {
		l.fail(taskID, 0, err.Error())
		return
	}

	// Repetition is counted per run rather than across the task's whole history.
	// Every key below carries the workspace fingerprint, and the fingerprint a
	// step ran at is not recorded, so there is nothing from an earlier run to
	// count against. Nor is there much point: a resumed run that genuinely loops
	// still trips the limit within its own steps, and seeding the counters from
	// the trace made resuming a conceded task useless — it aborted on the first
	// step that echoed anything it had said before.
	seen := map[string]int{}

	step := len(prior)
	parseFailures := 0

	// Read once, so a run keeps the budget it started with and the message it
	// concedes with names the number it actually got.
	maxSteps := l.stepLimit()

	for i := 0; i < maxSteps; i++ {
		if stopped(ctx) {
			l.markStopped(taskID, step)
			return
		}

		task, err := l.store.GetTask(taskID)
		if err != nil {
			l.fail(taskID, step, err.Error())
			return
		}
		if task == nil {
			l.fail(taskID, step, "task no longer exists")
			return
		}
		if task.FinishFlag {
			return
		}

		trace, err := l.store.ListTraceSteps(taskID)
		if err != nil {
			l.fail(taskID, step, err.Error())
			return
		}

		step++
		// A continued task inherits a workspace that already has files in it;
		// listing them keeps the agent from rewriting work it could build on.
		existing, _ := ws.List()
		messages := buildMessages(task, ws.Root(), existing, trace, sandbox != nil)

		// Native tool calls are the primary path; once the model has produced
		// something unusable, retry under a forced JSON response format instead.
		opts := llm.ChatOptions{Tools: ToolDefs(sandbox != nil), Model: task.Model}
		if parseFailures > 0 {
			opts = llm.ChatOptions{ForceJSON: true, Model: task.Model}
		}

		resp, err := l.api().Chat(ctx, messages, opts)
		if err != nil {
			if stopped(ctx) {
				l.markStopped(taskID, step)
				return
			}
			l.fail(taskID, step, fmt.Sprintf("model call failed at step %d: %v", step, err))
			return
		}
		if stopped(ctx) {
			l.markStopped(taskID, step)
			return
		}

		result, err := parseResponse(resp)
		if err != nil {
			parseFailures++
			l.store.AddTraceStep(taskID, step, actionParseFailure, "", resp.Content, "", parseFailureFeedback(err))
			if parseFailures >= parseFailureLimit {
				l.concede(taskID, step, fmt.Sprintf("model returned an unusable response %d times in a row: %v", parseFailures, err))
				return
			}
			continue
		}
		parseFailures = 0

		if result.GoalMet && len(result.Tools) == 0 {
			l.store.AddTraceStep(taskID, step, "goal met", "", result.Text, "", "")
			l.finish(taskID, step, result.Summary)
			return
		}

		action := result.Action
		logged := loggedAction(action, result.Tools)

		// What makes a repeat a loop is that nothing moved between the two
		// attempts, so every key that is not itself a change to the workspace is
		// qualified by the state it ran against. Re-reading a file you just
		// edited, or re-running the tests that caught a bug, is the loop working
		// as intended and must not be aborted — that is the one case where the
		// agent is converging.
		fingerprint := ws.Fingerprint()

		// A turn that signs off is the last one either way, so there is nothing
		// left for the guard to save it from.
		if !result.GoalMet {
			for _, pc := range result.Tools {
				key := "tool\x00" + pc.Call.signature()
				if !mutatingTool(pc.Call.Name) {
					key += "\x00" + fingerprint
				}
				seen[key]++
				if seen[key] >= repeatLimit {
					l.store.AddTraceStep(taskID, step, logged, "", result.Text, pc.Call.Name, "aborted: identical tool call repeated")
					l.concede(taskID, step, fmt.Sprintf("agent repeated the same %s call %d times without making progress", pc.Call.Name, seen[key]))
					return
				}
			}

			// Only count actions the model actually wrote. A synthesized label
			// describes the calls, which the tool signatures above already cover.
			if key := normalize(action); key != "" && !result.Synthesized {
				key += "\x00" + fingerprint
				seen[key]++
				if seen[key] >= repeatLimit {
					l.store.AddTraceStep(taskID, step, logged, "", result.Text, "", "aborted: identical action repeated")
					l.concede(taskID, step, fmt.Sprintf("agent repeated the same action %d times without making progress: %q", seen[key], action))
					return
				}
			}
		}

		// Calls run in the order the model made them, and one failing does not
		// cancel the rest: every call has to come back with a result of its own,
		// or the model is left holding an id nothing answered.
		exchanges := make([]models.ToolExchange, 0, len(result.Tools))
		for _, pc := range result.Tools {
			out, err := ws.ExecContext(ctx, pc.Call)
			if err != nil {
				out = "error: " + err.Error()
			}
			exchanges = append(exchanges, models.ToolExchange{
				ID:        pc.ID,
				Name:      pc.Call.Name,
				Arguments: replayArgs(pc.Args),
				Result:    out,
			})
		}

		toolName, toolResult := summarizeExchanges(exchanges)
		l.store.AddTrace(store.TraceEntry{
			TaskID: taskID, Step: step, Action: logged, Response: result.Text,
			ToolName: toolName, ToolResult: toolResult, Calls: exchanges,
		})

		if result.GoalMet {
			l.finish(taskID, step, result.Summary)
			return
		}
	}

	l.concede(taskID, step, fmt.Sprintf("reached the %d step limit without meeting the goal", maxSteps))
}

// finish files a run the model signed off on.
func (l *Loop) finish(taskID string, step int, summary string) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		summary = fmt.Sprintf("Task completed in %d steps.", step)
	}
	if err := l.settleRun(taskID, summary); err != nil {
		l.fail(taskID, step, err.Error())
	}
}

// settleRun files a run that produced something, in the column the server is
// configured to send it to. The agent's own sign-off is the same event either
// way; what review changes is whether it is the last word.
func (l *Loop) settleRun(taskID, summary string) error {
	if l.reviewEnabled() {
		return l.store.SetTaskInReview(taskID, summary)
	}
	return l.store.SetTaskFinished(taskID, summary)
}

// summarizeExchanges renders a batch of calls into the single name and result
// the trace displays. Everything that only shows a step — the board, the CLI —
// reads those two fields, so a turn that made three calls has to say so there
// rather than only in Calls.
func summarizeExchanges(ex []models.ToolExchange) (string, string) {
	switch len(ex) {
	case 0:
		return "", ""
	case 1:
		return ex[0].Name, ex[0].Result
	}
	parts := make([]string, 0, len(ex))
	for _, e := range ex {
		parts = append(parts, e.Name+": "+e.Result)
	}
	return fmt.Sprintf("%s +%d", ex[0].Name, len(ex)-1), strings.Join(parts, "\n\n")
}

// replayArgs caps the arguments carried back into the next request. A whole
// file's content sits in a write_file call, and replaying every one of them
// verbatim fills the window with bytes the model can read back off disk when it
// actually needs them. The clipped form is still a valid JSON object, since a
// provider may parse what it is handed rather than pass it through.
func replayArgs(args string) string {
	if len(args) <= toolCallBudget {
		return args
	}
	return argsNote(len(args))
}

// argsNote stands in for arguments that were dropped whole, as a JSON object so
// a provider that parses what it is handed still gets something valid.
func argsNote(n int) string {
	note, err := json.Marshal(map[string]string{
		"note": fmt.Sprintf("arguments too large to keep in the transcript (%d bytes); read the file back if you need them", n),
	})
	if err != nil {
		return "{}"
	}
	return string(note)
}

func (l *Loop) fail(taskID string, step int, msg string) {
	l.store.AddTraceStep(taskID, step, "run failed", "", "", "", msg)
	l.store.SetTaskStatus(taskID, models.StatusError, msg)
}

// concede ends a run that stopped making progress rather than one that broke.
// The three self-imposed guards — repetition, unusable output, the step limit —
// all mean the agent ran out of road, which is not the same as producing
// nothing. The common shape is a model that writes a working file and then
// loops re-reading it because it never realises it should call finish; filing
// that as an error buries a good deliverable behind a red status.
//
// So its own output decides. Files it wrote means the run is judged on what it
// built, with the reason recorded on the trace and in the summary. Having
// written nothing means it really did fail, and it still errors. What it wrote
// is not what the workspace holds — see producedFiles.
func (l *Loop) concede(taskID string, step int, reason string) {
	files, err := l.producedFiles(taskID)
	if err != nil || len(files) == 0 {
		l.fail(taskID, step, reason)
		return
	}
	l.store.AddTraceStep(taskID, step, "run ended without finish", "", "", "", reason)
	// A conceded run goes to review like any other. It is the run most likely to
	// have produced something half-built, which is an argument for checking it
	// rather than an argument for filing it unchecked.
	l.settleRun(taskID, concededSummary(files, step, reason))
}

// concededMark opens the summary of every conceded run. It is what separates
// work an agent signed off on from work it ran out of road on — the two arrive
// in the review column looking alike, and the reviewer is told which it holds.
const concededMark = "Stopped after"

// concededSummary says plainly that the agent never called finish, so a task
// that reads as done is not mistaken for one the model signed off on.
func concededSummary(files []FileEntry, step int, reason string) string {
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Path)
	}
	noun := "files"
	if len(names) == 1 {
		noun = "file"
	}
	return fmt.Sprintf(
		"%s %d steps without calling finish (%s). The workspace holds %d %s: %s.",
		concededMark, step, reason, len(names), noun, strings.Join(names, ", "),
	)
}

func (l *Loop) markStopped(taskID string, step int) {
	l.store.AddTraceStep(taskID, step, "run stopped by user", "", "", "", "")
	l.store.SetTaskStatus(taskID, models.StatusStopped, "")
}

func stopped(ctx context.Context) bool {
	return ctx.Err() != nil
}

func buildMessages(task *models.Task, workspace string, existing []FileEntry, trace []models.TraceStep, sandboxed bool) []llm.MsgBlock {
	var intro strings.Builder
	fmt.Fprintf(&intro, "Task goal: %s\n", task.Goal)
	if task.Description != "" {
		fmt.Fprintf(&intro, "Details: %s\n", task.Description)
	}
	// The agent is told what it will be judged against, in the same words the
	// reviewer gets. Withholding them would only mean discovering at review time
	// that the work was aimed somewhere else.
	if c := criteriaList(task.Criteria); c != "" {
		fmt.Fprintf(&intro, "\nYour output will be reviewed against these, by an agent that did not watch you work:\n%s\nMeeting them is the goal; anything else you add is beside the point.\n", c)
	}
	fmt.Fprintf(&intro, "Workspace directory: %s\n", workspace)

	// Only worth spelling out when the run did not start the workspace empty.
	if len(existing) > 0 && len(trace) == 0 {
		intro.WriteString("\nThis workspace already holds work from an earlier task:\n")
		for _, f := range existing {
			fmt.Fprintf(&intro, "  %s (%d bytes)\n", f.Path, f.Size)
		}
		intro.WriteString("Read what you need with read_file and build on it. Do not start over unless the goal asks you to.\n")
	}

	intro.WriteString("\nWhat is your next action?")

	system := systemPrompt
	if sandboxed {
		system += shellPrompt
	}

	msgs := []llm.MsgBlock{
		{Role: "system", Content: system},
		{Role: "user", Content: intro.String()},
	}

	return append(msgs, replayTrace(authorSteps(trace))...)
}

// authorSteps drops the steps a review pass wrote. They are recorded on the task
// so the verdict is visible where the work is, but replaying them to the author
// would hand it a critique of its own output as an assistant turn it had made —
// so the model reads its own name on the findings and argues with them.
func authorSteps(trace []models.TraceStep) []models.TraceStep {
	out := make([]models.TraceStep, 0, len(trace))
	for _, ts := range trace {
		if !strings.HasPrefix(ts.Action, reviewPrefix) {
			out = append(out, ts)
		}
	}
	return out
}

// replayTrace renders the trace under transcriptBudget. It walks backwards, so
// the budget is spent on the steps nearest the decision the model is about to
// make, and everything older is condensed.
func replayTrace(trace []models.TraceStep) []llm.MsgBlock {
	byStep := make([][]llm.MsgBlock, len(trace))
	used := 0
	for i := len(trace) - 1; i >= 0; i-- {
		full := used < transcriptBudget || len(trace)-i <= minFullSteps
		byStep[i] = replayStep(trace[i], full)
		used += blocksSize(byStep[i])
	}

	msgs := []llm.MsgBlock{}
	for _, blocks := range byStep {
		msgs = append(msgs, blocks...)
	}
	return msgs
}

// replayStep renders one trace step. A condensed step keeps its shape and loses
// its bulk: the same messages in the same order, so no assistant turn is left
// holding a tool call whose result went missing.
func replayStep(ts models.TraceStep, full bool) []llm.MsgBlock {
	// A step that made real tool calls replays as the exchange it was: the
	// assistant turn carrying those calls, then one "tool" message per call.
	// This is the shape models are trained on, and it is the only shape in
	// which the model can see the arguments it sent — without it, an agent
	// that wrote a file has no record of what it put in it and re-reads the
	// file to find out, which is the loop the repeat guard then aborts.
	if replayable(ts) {
		msgs := []llm.MsgBlock{assistantTurn(ts, full)}
		for _, c := range ts.Calls {
			content := truncate(c.Result, toolResultBudget)
			if !full {
				content = digest(c.Result)
			}
			msgs = append(msgs, llm.MsgBlock{
				Role:       "tool",
				ToolCallID: c.ID,
				Name:       c.Name,
				Content:    content,
			})
		}
		return msgs
	}

	msgs := []llm.MsgBlock{}
	if ts.Response != "" {
		content := ts.Response
		// Replaying a malformed reply in full invites the model to repeat it.
		if ts.Action == actionParseFailure {
			content = truncate(content, 500)
		}
		if !full {
			content = digest(content)
		}
		msgs = append(msgs, llm.MsgBlock{Role: "assistant", Content: content})
	}

	result := truncate(ts.ToolResult, toolResultBudget)
	if !full {
		result = digest(ts.ToolResult)
	}
	feedback := "Continue. What is your next action?"
	if ts.ToolName != "" {
		feedback = fmt.Sprintf("Your %s call returned:\n%s\n\nContinue. What is your next action?", ts.ToolName, result)
	} else if ts.ToolResult != "" {
		feedback = fmt.Sprintf("%s\n\nContinue. What is your next action?", result)
	}
	return append(msgs, llm.MsgBlock{Role: "user", Content: feedback})
}

// blocksSize is what a step costs the transcript. Tool call arguments count:
// a write_file's content sits in them, and they are most of a large step.
func blocksSize(msgs []llm.MsgBlock) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
		for _, c := range m.ToolCalls {
			n += len(c.Function.Arguments) + len(c.Function.Name)
		}
	}
	return n
}

// digest keeps the head of a value and says how much it dropped. The head is
// where a tool result identifies itself — the path written, the first compiler
// error — so it stays recognisable as the step it was.
func digest(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= digestBytes {
		return s
	}
	return fmt.Sprintf("%s\n...[%d more bytes from an earlier step, elided to make room — read the file back if you need them]",
		s[:digestBytes], len(s)-digestBytes)
}

// condenseArgs shrinks a call's arguments by field rather than by byte offset.
// The structure is what the model needs from an old call — which path it wrote,
// which command it ran — and the file content it also carries is the part that
// costs. Clipping the string would leave invalid JSON, which some providers
// parse rather than pass through.
func condenseArgs(args string) string {
	if len(args) <= digestBytes {
		return args
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(args), &fields); err != nil {
		return argsNote(len(args))
	}
	for k, v := range fields {
		if s, ok := v.(string); ok && len(s) > digestBytes {
			fields[k] = fmt.Sprintf("[%d bytes elided]", len(s))
		}
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return argsNote(len(args))
	}
	return string(out)
}

// replayable reports whether a step can go back as a native exchange. Every call
// needs the id its result will be keyed on: a "tool" message naming a call the
// assistant turn never made is rejected outright by some providers and silently
// unmatched by the rest, so a step missing any id replays as prose instead —
// which is also what every row written before calls were recorded does.
func replayable(ts models.TraceStep) bool {
	if len(ts.Calls) == 0 {
		return false
	}
	for _, c := range ts.Calls {
		if c.ID == "" || c.Name == "" {
			return false
		}
	}
	return true
}

// assistantTurn rebuilds the turn that made a step's calls. Content is whatever
// the model wrote alongside them, which is often nothing.
func assistantTurn(ts models.TraceStep, full bool) llm.MsgBlock {
	calls := make([]llm.ToolCall, 0, len(ts.Calls))
	for _, c := range ts.Calls {
		args := c.Arguments
		if !full {
			args = condenseArgs(args)
		}
		if args == "" {
			args = "{}"
		}
		calls = append(calls, llm.ToolCall{
			ID:       c.ID,
			Type:     "function",
			Function: llm.FunctionCall{Name: c.Name, Arguments: args},
		})
	}
	content := ts.Response
	if !full {
		content = digest(content)
	}
	return llm.MsgBlock{Role: "assistant", Content: content, ToolCalls: calls}
}

// pendingCall is one tool call the model asked for, with the identity the
// provider gave it. The id is what lets the result go back as a "tool" message
// the model can match to its own call; the JSON fallback protocol has no ids, so
// a call parsed from it replays as prose instead.
type pendingCall struct {
	ID   string
	Args string
	Call ToolCall
}

// stepResult is one decoded model turn, from native tool calls or the JSON
// fallback body.
type stepResult struct {
	GoalMet bool
	Summary string
	Action  string
	// Tools holds every call the turn made, in the order the model made them.
	// Models routinely emit several at once — three files written in one turn —
	// and running only the first leaves the model believing in two writes that
	// never happened.
	Tools []pendingCall
	// Text is replayed as the assistant turn in later requests. It holds only
	// what the model actually wrote — replaying a synthesized description of a
	// tool call teaches the model to emit that description instead of calling.
	Text string
	// Synthesized reports that Action was written by us, not the model.
	Synthesized bool
}

// parseResponse prefers native tool calls and falls back to the JSON protocol
// for models that ignore the tools parameter.
func parseResponse(resp *llm.Result) (*stepResult, error) {
	content := strings.TrimSpace(resp.Content)

	if len(resp.ToolCalls) > 0 {
		var calls []pendingCall
		goalMet, summary := false, ""

		for _, call := range resp.ToolCalls {
			name := strings.ToLower(strings.TrimSpace(call.Function.Name))
			if name == "" {
				return nil, fmt.Errorf("tool call had no function name")
			}

			var tc ToolCall
			args := strings.TrimSpace(call.Function.Arguments)
			if args != "" {
				if err := json.Unmarshal([]byte(args), &tc); err != nil {
					return nil, fmt.Errorf("tool call arguments for %s were not valid JSON: %v", name, err)
				}
			}
			tc.Name = name

			// finish arrives alongside the work it signs off on often enough that
			// treating it as the whole turn discards that work. It is remembered
			// and applied once the calls beside it have run.
			if name == finishTool {
				goalMet = true
				if summary == "" {
					summary = tc.Summary
				}
				continue
			}
			calls = append(calls, pendingCall{ID: call.ID, Args: args, Call: tc})
		}

		if goalMet && len(calls) == 0 {
			return &stepResult{GoalMet: true, Summary: summary, Text: content}, nil
		}

		action, synthesized := content, false
		if action == "" {
			action, synthesized = describeCalls(calls), true
		}
		return &stepResult{
			GoalMet:     goalMet,
			Summary:     summary,
			Action:      action,
			Tools:       calls,
			Text:        content,
			Synthesized: synthesized,
		}, nil
	}

	body, ok := extractJSON(content)
	if !ok {
		return nil, fmt.Errorf("no tool call and no JSON object in the response")
	}
	var parsed struct {
		GoalMet    bool      `json:"goal_met"`
		Summary    string    `json:"summary"`
		NextAction string    `json:"next_action"`
		Tool       *ToolCall `json:"tool"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return nil, fmt.Errorf("JSON parse error: %v", err)
	}

	action := strings.TrimSpace(parsed.NextAction)
	synthesized := false
	if action == "" {
		action, synthesized = "(no action described)", true
	}
	var tools []pendingCall
	if parsed.Tool != nil {
		tools = []pendingCall{{Call: *parsed.Tool}}
	}
	return &stepResult{
		GoalMet:     parsed.GoalMet,
		Summary:     parsed.Summary,
		Action:      action,
		Tools:       tools,
		Text:        content,
		Synthesized: synthesized,
	}, nil
}

// parseFailureFeedback restates the contract instead of echoing a bare decoder
// error, which models tend to ignore.
func parseFailureFeedback(err error) string {
	return fmt.Sprintf(`Your last reply could not be used: %v

Do not describe a tool call in prose, XML, or <tool_call> tags. Either make a real
tool call, or reply with exactly one JSON object and nothing else:

  {"goal_met": false, "next_action": "...", "tool": {"name":"write_file","path":"index.html","content":"..."}}

File contents go inside the "content" string, JSON-escaped — newlines as \n and
quotes as \". Send the corrected reply now.`, err)
}

// loggedAction is what the trace records: the model's own words, with any
// command it ran named alongside them. describeCalls only names the command when
// the model wrote nothing itself, and most of the time it writes something — so
// without this the operator reads a failed step that never says what was run.
func loggedAction(action string, calls []pendingCall) string {
	for _, pc := range calls {
		if pc.Call.Command == "" {
			continue
		}
		ran := "[ran: " + truncate(pc.Call.Command, 200) + "]"
		if strings.Contains(action, ran) {
			continue
		}
		action = strings.TrimSpace(action + " " + ran)
	}
	return action
}

// describeCalls labels a turn that made calls without saying anything about
// them.
func describeCalls(calls []pendingCall) string {
	if len(calls) == 0 {
		return "(no action described)"
	}
	parts := make([]string, 0, len(calls))
	for _, pc := range calls {
		parts = append(parts, describeCall(pc.Call))
	}
	return strings.Join(parts, " ")
}

func describeCall(tc ToolCall) string {
	if tc.Command != "" {
		return fmt.Sprintf("[ran: %s]", truncate(tc.Command, 200))
	}
	if tc.Path != "" {
		return fmt.Sprintf("[called %s on %s]", tc.Name, tc.Path)
	}
	return fmt.Sprintf("[called %s]", tc.Name)
}

// agentKeys are the envelope fields that distinguish a real response object
// from an incidental one, such as a JS object literal inside file content.
var agentKeys = []string{"goal_met", "next_action", "tool", "summary"}

// maxJSONCandidates bounds the scan on large responses.
const maxJSONCandidates = 200

// extractJSON returns the first balanced {...} span that decodes as an agent
// response.
func extractJSON(s string) (string, bool) {
	return extractJSONWith(s, agentKeys)
}

// extractJSONWith is extractJSON parameterised by the envelope fields that mark
// a candidate as the object being looked for — a step reply and a breakdown are
// both one object wrapped in fences and commentary, but they are recognised by
// different keys.
//
// It scans candidates rather than taking first-{ through last-}, which would
// swallow braces in CSS or JS the model is writing to a file.
func extractJSONWith(s string, keys []string) (string, bool) {
	s = stripFences(s)
	tried := 0
	for i := 0; i < len(s) && tried < maxJSONCandidates; i++ {
		if s[i] != '{' {
			continue
		}
		tried++
		end, ok := matchBrace(s, i)
		if !ok {
			continue
		}
		candidate := s[i : end+1]
		if hasAnyKey(candidate, keys) {
			return candidate, true
		}
	}
	return "", false
}

func stripFences(s string) string {
	s = strings.TrimSpace(s)
	fence := strings.Index(s, "```")
	if fence < 0 {
		return s
	}
	rest := s[fence+3:]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	}
	if end := strings.Index(rest, "```"); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}

// matchBrace finds the '}' closing the '{' at start, ignoring braces that sit
// inside JSON string literals.
func matchBrace(s string, start int) (int, bool) {
	depth, inString, escaped := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// Braces inside a string are content, not structure.
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

func hasAnyKey(candidate string, keys []string) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(candidate), &probe); err != nil {
		return false
	}
	for _, k := range keys {
		if _, ok := probe[k]; ok {
			return true
		}
	}
	return false
}

func normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n...[truncated]"
}
