package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"fanoutd/internal/models"
	"fanoutd/internal/openrouter"
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

var ErrAlreadyRunning = errors.New("agent loop already running for this task")

var ErrGroupRunning = errors.New("this breakdown is already running")

// ErrGroupNotFound is a group id with no subtasks behind it. Groups have no
// table of their own, so this is the only way one can be missing.
var ErrGroupNotFound = errors.New("no such group")

// defaultMaxParallel caps how many subtasks of one breakdown run at once. The
// binding constraint is the provider, not the machine: a dozen concurrent
// agents on one OpenRouter key earn rate limits rather than throughput.
const defaultMaxParallel = 3

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
Each step must make new progress.`

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
Only your workspace and /build are writable. Compiler and package caches already go
outside the workspace, but a binary lands wherever you tell it to: build to /build, as in
"go build -o /build/tool .", so your workspace keeps the source you were asked for rather
than a megabyte of compiled output.

Long-running commands are killed, so prefer targeted builds and tests over whole-repo
ones. Re-running a command you have already run, unchanged, is not progress.`

type Loop struct {
	store   *store.Store
	client  *openrouter.Client
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
}

// SetSandbox enables the shell tool. A nil sandbox leaves agents file-only.
func (l *Loop) SetSandbox(sb *Sandbox) {
	l.mu.Lock()
	l.sandbox = sb
	l.mu.Unlock()
}

func NewLoop(s *store.Store, c *openrouter.Client, outputDir string) *Loop {
	return &Loop{
		store:       s,
		client:      c,
		cancels:     make(map[string]context.CancelFunc),
		groups:      make(map[string]context.CancelFunc),
		maxSteps:    20,
		maxParallel: defaultMaxParallel,
		outputDir:   outputDir,
	}
}

// SetMaxParallel bounds concurrent subtasks within one breakdown. Values below
// one are ignored.
func (l *Loop) SetMaxParallel(n int) {
	if n < 1 {
		return
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

	l.mu.Lock()
	if _, running := l.cancels[taskID]; running {
		l.mu.Unlock()
		return nil, ErrAlreadyRunning
	}
	ctx, cancel := context.WithCancel(parent)
	l.cancels[taskID] = cancel
	l.mu.Unlock()

	if err := l.store.SetTaskColumn(taskID, "todo"); err != nil {
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
	}()
	return done, nil
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
	cancels := make([]context.CancelFunc, 0, len(l.cancels)+len(l.groups))
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

// WorkspaceFiles lists the files the agent has produced for a task.
func (l *Loop) WorkspaceFiles(taskID string) ([]FileEntry, error) {
	ws, err := l.Workspace(taskID)
	if err != nil {
		return nil, err
	}
	return ws.List()
}

// producedFiles narrows a workspace listing to the files this task itself
// wrote. Subtasks of one breakdown share a workspace, so the raw listing credits
// every sibling's output to whoever asks — which is the difference between
// conceding to done and conceding to error. Ownership is only recorded for
// grouped tasks, and a solo task has the workspace to itself, so for those the
// listing already is what it produced.
func (l *Loop) producedFiles(taskID string) ([]FileEntry, error) {
	files, err := l.WorkspaceFiles(taskID)
	if err != nil {
		return nil, err
	}
	task, err := l.store.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	if task == nil || task.GroupID == "" {
		return files, nil
	}

	owned, err := l.store.OwnedPaths(workspaceID(task), taskID)
	if err != nil {
		return nil, err
	}
	mine := make(map[string]bool, len(owned))
	for _, p := range owned {
		mine[p] = true
	}

	var out []FileEntry
	for _, f := range files {
		if key, ok := normalizeClaimPath(f.Path); ok && mine[key] {
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

	// Repetition counters carry over from earlier runs of the same task so a
	// stop/start cycle cannot be used to loop forever.
	seen := map[string]int{}
	for _, ts := range prior {
		seen[normalize(ts.Action)]++
	}

	step := len(prior)
	parseFailures := 0

	for i := 0; i < l.maxSteps; i++ {
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
		opts := openrouter.ChatOptions{Tools: ToolDefs(sandbox != nil), Model: task.Model}
		if parseFailures > 0 {
			opts = openrouter.ChatOptions{ForceJSON: true, Model: task.Model}
		}

		resp, err := l.client.Chat(ctx, messages, opts)
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

		if result.GoalMet {
			summary := strings.TrimSpace(result.Summary)
			if summary == "" {
				summary = fmt.Sprintf("Task completed in %d steps.", step)
			}
			l.store.AddTraceStep(taskID, step, "goal met", "", result.Text, "", "")
			if err := l.store.SetTaskFinished(taskID, summary); err != nil {
				l.fail(taskID, step, err.Error())
			}
			return
		}

		action := result.Action
		logged := loggedAction(action, result.Tool)

		if result.Tool != nil {
			key := "tool\x00" + result.Tool.signature()
			// A command is judged against the files it runs on, not on its text
			// alone. Re-running the tests after fixing what they caught is the
			// loop working as intended; without this the guard aborts an agent
			// that is converging, which is the one case it should leave alone.
			if result.Tool.Name == execTool {
				key += "\x00" + ws.Fingerprint()
			}
			seen[key]++
			if seen[key] >= repeatLimit {
				l.store.AddTraceStep(taskID, step, logged, "", result.Text, result.Tool.Name, "aborted: identical tool call repeated")
				l.concede(taskID, step, fmt.Sprintf("agent repeated the same %s call %d times without making progress", result.Tool.Name, seen[key]))
				return
			}
		}

		// Only count actions the model actually wrote. A synthesized label
		// describes the call, which the tool signature above already covers.
		if key := normalize(action); key != "" && !result.Synthesized {
			seen[key]++
			if seen[key] >= repeatLimit {
				l.store.AddTraceStep(taskID, step, logged, "", result.Text, "", "aborted: identical action repeated")
				l.concede(taskID, step, fmt.Sprintf("agent repeated the same action %d times without making progress: %q", seen[key], action))
				return
			}
		}

		toolName, toolResult := "", ""
		if result.Tool != nil {
			toolName = result.Tool.Name
			out, err := ws.ExecContext(ctx, *result.Tool)
			if err != nil {
				toolResult = "error: " + err.Error()
			} else {
				toolResult = out
			}
		}

		l.store.AddTraceStep(taskID, step, logged, "", result.Text, toolName, toolResult)

		select {
		case <-ctx.Done():
			l.markStopped(taskID, step)
			return
		case <-time.After(500 * time.Millisecond):
		}
	}

	l.concede(taskID, step, fmt.Sprintf("reached the %d step limit without meeting the goal", l.maxSteps))
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
	l.store.SetTaskFinished(taskID, concededSummary(files, step, reason))
}

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
		"Stopped after %d steps without calling finish (%s). The workspace holds %d %s: %s.",
		step, reason, len(names), noun, strings.Join(names, ", "),
	)
}

func (l *Loop) markStopped(taskID string, step int) {
	l.store.AddTraceStep(taskID, step, "run stopped by user", "", "", "", "")
	l.store.SetTaskStatus(taskID, models.StatusStopped, "")
}

func stopped(ctx context.Context) bool {
	return ctx.Err() != nil
}

func buildMessages(task *models.Task, workspace string, existing []FileEntry, trace []models.TraceStep, sandboxed bool) []openrouter.MsgBlock {
	var intro strings.Builder
	fmt.Fprintf(&intro, "Task goal: %s\n", task.Goal)
	if task.Description != "" {
		fmt.Fprintf(&intro, "Details: %s\n", task.Description)
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

	msgs := []openrouter.MsgBlock{
		{Role: "system", Content: system},
		{Role: "user", Content: intro.String()},
	}

	for _, ts := range trace {
		if ts.Response != "" {
			content := ts.Response
			// Replaying a malformed reply in full invites the model to repeat it.
			if ts.Action == actionParseFailure {
				content = truncate(content, 500)
			}
			msgs = append(msgs, openrouter.MsgBlock{Role: "assistant", Content: content})
		}
		feedback := "Continue. What is your next action?"
		if ts.ToolName != "" {
			feedback = fmt.Sprintf("Your %s call returned:\n%s\n\nContinue. What is your next action?", ts.ToolName, truncate(ts.ToolResult, toolResultBudget))
		} else if ts.ToolResult != "" {
			feedback = fmt.Sprintf("%s\n\nContinue. What is your next action?", ts.ToolResult)
		}
		msgs = append(msgs, openrouter.MsgBlock{Role: "user", Content: feedback})
	}

	return msgs
}

// stepResult is one decoded model turn, from either a native tool call or the
// JSON fallback body.
type stepResult struct {
	GoalMet bool
	Summary string
	Action  string
	Tool    *ToolCall
	// Text is replayed as the assistant turn in later requests. It holds only
	// what the model actually wrote — replaying a synthesized description of a
	// tool call teaches the model to emit that description instead of calling.
	Text string
	// Synthesized reports that Action was written by us, not the model.
	Synthesized bool
}

// parseResponse prefers a native tool call and falls back to the JSON protocol
// for models that ignore the tools parameter.
func parseResponse(resp *openrouter.Result) (*stepResult, error) {
	content := strings.TrimSpace(resp.Content)

	if len(resp.ToolCalls) > 0 {
		call := resp.ToolCalls[0]
		name := strings.ToLower(strings.TrimSpace(call.Function.Name))
		if name == "" {
			return nil, fmt.Errorf("tool call had no function name")
		}

		var tc ToolCall
		if args := strings.TrimSpace(call.Function.Arguments); args != "" {
			if err := json.Unmarshal([]byte(args), &tc); err != nil {
				return nil, fmt.Errorf("tool call arguments for %s were not valid JSON: %v", name, err)
			}
		}
		tc.Name = name

		if name == finishTool {
			return &stepResult{GoalMet: true, Summary: tc.Summary, Text: content}, nil
		}

		action, synthesized := content, false
		if action == "" {
			action, synthesized = describeCall(tc), true
		}
		return &stepResult{
			Action:      action,
			Tool:        &tc,
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
	return &stepResult{
		GoalMet:     parsed.GoalMet,
		Summary:     parsed.Summary,
		Action:      action,
		Tool:        parsed.Tool,
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

// loggedAction is what the trace records. describeCall only names the command
// when the model wrote no action text of its own, and most of the time it does
// — so without this the command is recorded nowhere, since native tool call
// arguments are never replayed. That leaves the operator reading an
// unexplained failure and the model reading output it cannot attribute.
func loggedAction(action string, tc *ToolCall) string {
	if tc == nil || tc.Command == "" {
		return action
	}
	ran := "[ran: " + truncate(tc.Command, 200) + "]"
	if strings.Contains(action, ran) {
		return action
	}
	return strings.TrimSpace(action + " " + ran)
}

func describeCall(tc ToolCall) string {
	if tc.Command != "" {
		// The command has to appear here or it is recorded nowhere: native tool
		// call arguments are not replayed, so without this the model reads back
		// its own output with no idea what produced it, and the trace shows an
		// unexplained failure.
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
