package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path"
	"sort"
	"strconv"
	"strings"

	"fanoutd/internal/models"
	"fanoutd/internal/openrouter"
	"fanoutd/internal/store"
)

// Turning one idea into a group is the front half of a breakdown; schedule.go is
// the back half. Everything here happens before a single task row exists, which
// is deliberate: a plan that cannot be run must cost nothing to reject.

// A breakdown must split the idea in two or more ways to be worth running, and
// past maxSubtasks it is shredding the work rather than dividing it — every
// extra subtask is another agent with less context about the whole.
const (
	minSubtasks = 2
	maxSubtasks = 8
)

// replanAttempts is the total number of model calls one breakdown may cost: the
// plan, and one correction with the fault named. A model that cannot partition
// the files in two tries is not going to on the third, and the fallback is
// always available.
const replanAttempts = 2

// breakdownEcho caps how much of a rejected plan is replayed in the retry. Long
// enough to show the model its own structure, short enough that a plan with
// eight verbose goals does not crowd out the correction.
const breakdownEcho = 4000

// ideaTitleLen is how much of an idea becomes the fallback task's title.
const ideaTitleLen = 60

// breakdownKeys is the envelope field that identifies a breakdown object inside
// whatever prose or fences the model wrapped it in.
var breakdownKeys = []string{"subtasks"}

// breakdownPrompt is where the work of this feature actually is. The scheduler
// and the claims are mechanical once the file partition is right; the partition
// is the part a model has to get right, so the prompt is written around that one
// job rather than around describing the output format.
const breakdownPrompt = `You split one idea into subtasks that separate agents will run at the same time, in one shared directory.

Reply with a single JSON object and nothing else — no prose, no code fences:

{"subtasks": [
  {"title": "short label",
   "goal": "a complete, self-contained brief for one agent",
   "writes": ["path/it/creates.ext"],
   "reads": ["path/a/sibling/creates.ext"]}
]}

The two file lists are the whole design. Decide them first and write the goals
second, because they carry both things that make a breakdown work:

- Ownership. A path has exactly one writer. Two subtasks naming the same path in
  "writes" is the single failure that makes a plan unrunnable — the second agent
  is refused the file at the moment it tries to write it.
- Ordering. A subtask that names another's output in "reads" runs after it, and
  starts with that file already on disk. This is the only way to express order.
  Do not write "after step 2", do not number the subtasks, and do not describe
  dependencies in a goal. If B needs what A produced, B reads A's file.

Rules:
- Between 2 and 6 subtasks. Split where the files split. If the idea produces
  two files, a two-way split is the right answer and a five-way split is not.
  Never invent a file to justify another subtask.
- Every subtask writes at least one file. A subtask that writes nothing produces
  nothing and nothing can depend on it.
- Paths are relative to the shared directory: "src/board.js". Never "/src/board.js",
  never "./src/board.js", never "..".
- "reads" holds only paths that another subtask in this plan writes. A subtask
  starting from nothing has an empty list. Never list a path you also write.
- Reads must not run in a circle. If A reads a file B writes, B must not read
  anything A writes, directly or through a third subtask.
- One file that every subtask would otherwise touch — an index page, a router
  table, a manifest, a shared stylesheet — is not split. Give it to a single
  final integration subtask that writes it and reads the others' outputs. That
  subtask runs last on its own, which is exactly what you want for it.
- Each goal must stand on its own. The agent running it sees its goal and nothing
  else: not the original idea, not its siblings, not their goals. So state what
  it must build, what the files it reads will contain, and what the files it
  writes must contain, in full, in every goal.

Prefer a clean partition with fewer subtasks over a wide one that makes two
agents share a file.`

// Subtask is one piece of a broken-down idea, exactly as the model returns it.
type Subtask struct {
	Title  string   `json:"title"`
	Goal   string   `json:"goal"`
	Writes []string `json:"writes"`
	Reads  []string `json:"reads"`
}

// BreakdownRequest is one idea to split. Title is only used if the split fails
// and the idea has to be created as a single task.
//
// Seed is material to place in the shared workspace before anything runs. It is
// shown to the planner, so the split can be drawn around files that already
// exist rather than around files the subtasks must invent.
type BreakdownRequest struct {
	Title string
	Idea  string
	Model string
	Start bool
	Seed  []models.SeedFile
}

// Breakdown turns an idea into a running group, or into one ordinary task.
//
// There is always a result. A model that will not produce a usable partition, a
// cycle in the one it produced, even a claim lost to a race — each ends with the
// idea created as a single task carrying the original wording, with the reason
// in Fallback. Parallelism is the optimisation; running the idea at all is not.
func (l *Loop) Breakdown(ctx context.Context, req BreakdownRequest) (*models.BreakdownResult, error) {
	idea := strings.TrimSpace(req.Idea)
	if idea == "" {
		return nil, fmt.Errorf("an idea is required")
	}
	req.Idea = idea

	subs, err := l.planBreakdown(ctx, req)
	if err != nil {
		return l.singleTask(req, err)
	}
	result, err := l.buildGroup(req, subs)
	if err != nil {
		return l.singleTask(req, err)
	}
	return result, nil
}

// planBreakdown asks for a partition and gives the model exactly one chance to
// repair a bad one, with the fault named. The conversation carries the rejected
// plan so the correction is an edit rather than a fresh guess.
func (l *Loop) planBreakdown(ctx context.Context, req BreakdownRequest) ([]Subtask, error) {
	messages := []openrouter.MsgBlock{
		{Role: "system", Content: breakdownPrompt},
		{Role: "user", Content: "Idea: " + req.Idea + seedBrief(req.Seed) + "\n\nSplit it. Reply with the JSON object and nothing else."},
	}

	var last error
	for attempt := 0; attempt < replanAttempts; attempt++ {
		// No tools: this call wants one JSON object, which is the one thing
		// response_format is for. The client falls back on its own for providers
		// that reject it.
		resp, err := l.client.Chat(ctx, messages, openrouter.ChatOptions{ForceJSON: true, Model: req.Model})
		if err != nil {
			// A transport failure is not a bad plan; re-asking would only repeat it.
			return nil, fmt.Errorf("breakdown call failed: %w", err)
		}

		subs, err := parseBreakdown(resp.Content)
		if err == nil {
			err = validateBreakdown(subs)
		}
		if err == nil {
			return subs, nil
		}
		last = err
		messages = append(messages,
			openrouter.MsgBlock{Role: "assistant", Content: truncate(resp.Content, breakdownEcho)},
			openrouter.MsgBlock{Role: "user", Content: replanPrompt(err)},
		)
	}
	return nil, last
}

// replanPrompt states the fault and forbids the cheap way out of it. A model
// told only "two subtasks write board.js" will often drop one of them, which
// resolves the conflict by discarding the work.
func replanPrompt(err error) string {
	return fmt.Sprintf(`That plan cannot be run: %v

Send the whole object again with that fixed — every subtask, same JSON shape,
nothing else.

If two subtasks wanted one file, give the file to whichever subtask produces it
and have the other read it, or move it into a final integration subtask that
writes it and reads both their outputs. Do not fix it by deleting a subtask or
by dropping part of the work.`, err)
}

// parseBreakdown reads the plan out of the reply. Models fence their JSON and
// narrate around it, which the step parser already deals with; this reuses that
// scan, keyed on the field a breakdown carries instead.
func parseBreakdown(content string) ([]Subtask, error) {
	body, ok := extractJSONWith(content, breakdownKeys)
	if !ok {
		return nil, fmt.Errorf("the reply held no breakdown object")
	}
	var parsed struct {
		Subtasks []Subtask `json:"subtasks"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return nil, fmt.Errorf("the breakdown was not valid JSON: %v", err)
	}

	subs := make([]Subtask, 0, len(parsed.Subtasks))
	for _, s := range parsed.Subtasks {
		s.Goal = strings.TrimSpace(s.Goal)
		s.Title = strings.TrimSpace(s.Title)
		if s.Title == "" {
			// A missing label is not worth rejecting a good partition over.
			s.Title = ideaTitle(s.Goal)
		}
		s.Writes = normalizePaths(s.Writes)
		s.Reads = normalizePaths(s.Reads)
		if s.Goal == "" && len(s.Writes) == 0 {
			continue // an empty object in the array, not a subtask
		}
		subs = append(subs, s)
	}
	return subs, nil
}

// normalizePaths renders each path the way a claim will be keyed and drops the
// ones that cannot be claimed at all, preserving order and dropping repeats.
func normalizePaths(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, p := range in {
		key, ok := normalizeClaimPath(p)
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

// normalizeClaimPath is the same reduction Workspace.resolveOwned performs
// before it claims a path. Doing it here is what makes "board.js", "./board.js"
// and "/board.js" collide during validation rather than surviving it and
// colliding in the database, where the only remedy left is the fallback.
func normalizeClaimPath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", false
	}
	p = path.Clean(strings.TrimLeft(p, "/"))
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}

// validateBreakdown rejects a plan that cannot be run, phrasing the failure for
// the model rather than for a log: every message here is fed back verbatim in
// the retry, so it names the subtasks and paths at fault.
func validateBreakdown(subs []Subtask) error {
	if len(subs) == 0 {
		return fmt.Errorf("the plan held no subtasks")
	}
	if len(subs) < minSubtasks {
		return fmt.Errorf("the plan held a single subtask (%q); a breakdown needs at least %d, each owning different files",
			subs[0].Title, minSubtasks)
	}
	if len(subs) > maxSubtasks {
		return fmt.Errorf("the plan held %d subtasks, which is more than the %d this can run; merge the ones that write related files",
			len(subs), maxSubtasks)
	}

	for _, s := range subs {
		if s.Goal == "" {
			return fmt.Errorf("subtask %q has no goal", s.Title)
		}
		if len(s.Writes) == 0 {
			return fmt.Errorf("subtask %q writes no files, so it produces nothing and no other subtask can depend on it", s.Title)
		}
	}

	// The failure mode that matters. Two writers on one path means the loser is
	// refused mid-run, having already spent the tokens that got it there.
	if conflicts := writeConflicts(subs); len(conflicts) > 0 {
		return fmt.Errorf("two subtasks cannot write the same file: %s", describeConflicts(conflicts))
	}

	// A cycle is equally fatal and equally re-plannable, and catching it here
	// rather than in PlanGroup is the difference between a retry and a fallback.
	if titles := cycleTitles(subs); len(titles) > 0 {
		return fmt.Errorf("the reads run in a circle, so nothing can go first: %s. One of them must start from files no other subtask writes",
			strings.Join(quoteAll(titles), ", "))
	}
	return nil
}

// writeConflict is one path more than one subtask claimed.
type writeConflict struct {
	Path   string
	Titles []string
}

// writeConflicts finds every contested path, sorted so the same bad plan always
// produces the same complaint.
func writeConflicts(subs []Subtask) []writeConflict {
	claimants := map[string][]string{}
	for _, s := range subs {
		for _, p := range s.Writes {
			claimants[p] = append(claimants[p], s.Title)
		}
	}

	out := []writeConflict{}
	for p, titles := range claimants {
		if len(titles) > 1 {
			out = append(out, writeConflict{Path: p, Titles: titles})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func describeConflicts(conflicts []writeConflict) string {
	parts := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		parts = append(parts, fmt.Sprintf("%s is claimed by %s", c.Path, strings.Join(quoteAll(c.Titles), " and ")))
	}
	return strings.Join(parts, "; ")
}

// cycleTitles runs the real dependency derivation over the proposed partition
// and reports the subtasks left unorderable. It uses the same deriveDeps and
// topoWaves the scheduler will, keyed on positions rather than task ids, so a
// plan that passes here cannot fail PlanGroup for a reason of its own.
func cycleTitles(subs []Subtask) []string {
	ids := make([]string, len(subs))
	titles := make(map[string]string, len(subs))
	writers := map[string]string{}
	reads := map[string][]string{}

	for i, s := range subs {
		ids[i] = strconv.Itoa(i)
		titles[ids[i]] = s.Title
		reads[ids[i]] = s.Reads
		for _, p := range s.Writes {
			if _, taken := writers[p]; !taken {
				writers[p] = ids[i]
			}
		}
	}

	_, err := topoWaves(ids, deriveDeps(ids, reads, writers))
	var cycle *cycleError
	if !errors.As(err, &cycle) {
		return nil
	}
	out := make([]string, 0, len(cycle.IDs))
	for _, id := range cycle.IDs {
		out = append(out, titles[id])
	}
	return out
}

func quoteAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strconv.Quote(s))
	}
	return out
}

// buildGroup creates the accepted plan: one group, one shared workspace, and the
// claims that make it a schedule. Nothing is left half-built — if the graph will
// not resolve, every task created here is deleted before returning, so the
// caller's fallback starts from a clean board.
func (l *Loop) buildGroup(req BreakdownRequest, subs []Subtask) (*models.BreakdownResult, error) {
	groupID, workspaceID := store.NewID(), store.NewID()
	created := []models.Task{}

	unwind := func() {
		for _, t := range created {
			// DeleteTask releases the task's claims as part of removing it, which
			// is what frees the paths for whatever is created next.
			if err := l.store.DeleteTask(t.ID); err != nil {
				log.Printf("could not unwind subtask %s of rejected breakdown: %v\n", shortID(t.ID), err)
			}
		}
	}

	for _, sub := range subs {
		task, err := l.store.CreateTaskFrom(store.NewTask{
			Title:       sub.Title,
			Description: subtaskContext(req.Idea),
			Goal:        sub.Goal,
			Model:       req.Model,
			WorkspaceID: workspaceID,
			GroupID:     groupID,
		})
		if err != nil {
			unwind()
			return nil, err
		}
		created = append(created, *task)

		// Validation already ruled these out, so a conflict here is a claim the
		// workspace picked up between then and now. Treating it as fatal rather
		// than proceeding keeps the invariant the scheduler relies on.
		conflicts, err := l.store.DeclareWrites(workspaceID, task.ID, sub.Writes)
		if err == nil && len(conflicts) > 0 {
			err = fmt.Errorf("%s was already claimed by task %s", conflicts[0].Path, shortID(conflicts[0].Owner))
		}
		if err == nil {
			err = l.store.DeclareReads(workspaceID, task.ID, sub.Reads)
		}
		if err != nil {
			unwind()
			return nil, err
		}
	}

	// Resolve the graph before anything runs. This is the cycle check the
	// scheduler would do at StartGroup, done early enough that failing it costs
	// no tokens.
	plan, err := l.PlanGroup(groupID)
	if err != nil {
		unwind()
		return nil, err
	}

	// Seeded last, so a group that does not survive validation never leaves a
	// half-installed workspace behind for unwind to explain.
	if err := l.seedWorkspace(workspaceID, req.Seed); err != nil {
		unwind()
		return nil, err
	}

	result := &models.BreakdownResult{
		GroupID: groupID,
		Tasks:   created,
		Plan: &models.GroupPlan{
			GroupID: groupID,
			Waves:   plan.Waves,
			Deps:    plan.Deps,
			Tasks:   created,
		},
	}
	if req.Start {
		if err := l.StartGroup(groupID); err != nil {
			// The group is built and valid; only the run failed to launch, and it
			// can be started again.
			log.Printf("built breakdown %s but could not start it: %v\n", shortID(groupID), err)
		} else {
			result.Started = true
			result.Plan.Running = true
			if view, err := l.GroupView(groupID); err == nil {
				result.Plan = view
				result.Tasks = view.Tasks
			}
		}
	}
	return result, nil
}

// ideaPrefix opens every subtask description, and is the only place the idea a
// group came from survives — there is no groups table, just a shared id on the
// tasks. GroupIdea reads it back out, so the two must be changed together.
const ideaPrefix = "This is one part of a larger idea, split across subtasks running in parallel: "

// subtaskContext reaches the model as "Details:". It says plainly that the rest
// of the idea belongs to somebody else — an agent that can see the whole idea
// will try to build the whole idea, and then lose every file it does not own.
func subtaskContext(idea string) string {
	return fmt.Sprintf(
		"%s%s\n"+
			"Do only what your goal above names. Sibling subtasks own the rest, and files belonging to them are refused if you try to write them.",
		ideaPrefix, truncate(idea, 400),
	)
}

// GroupIdea recovers the idea a subtask was split from, for a caller that wants
// to label the group rather than its parts. It returns "" for anything this
// package did not write, which a caller must be ready for: a group assembled by
// hand carries no idea, and one truncated at 400 characters is the wording the
// model was actually given rather than the wording typed.
func GroupIdea(description string) string {
	if !strings.HasPrefix(description, ideaPrefix) {
		return ""
	}
	idea, _, _ := strings.Cut(strings.TrimPrefix(description, ideaPrefix), "\n")
	return strings.TrimSpace(idea)
}

// singleTask is the floor. Every path that fails to produce a group lands here,
// with the original idea as the goal and the reason recorded, because a user who
// asked for work to happen would rather have it happen serially than not at all.
func (l *Loop) singleTask(req BreakdownRequest, cause error) (*models.BreakdownResult, error) {
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = ideaTitle(req.Idea)
	}
	task, err := l.store.CreateTaskFrom(store.NewTask{
		Title: title,
		Goal:  req.Idea,
		Model: req.Model,
	})
	if err != nil {
		return nil, err
	}
	// The seed was for the idea, not for the shape of the plan, so the fallback
	// gets it too. A task that cannot be seeded is still worth having.
	if err := l.seedWorkspace(task.WorkspaceID, req.Seed); err != nil {
		log.Printf("created fallback task %s but could not seed its workspace: %v\n", shortID(task.ID), err)
	}

	result := &models.BreakdownResult{
		Tasks:    []models.Task{*task},
		Fallback: fmt.Sprintf("could not split this into parallel subtasks, so it was created as one task: %v", cause),
	}
	if req.Start {
		if err := l.Start(task.ID); err != nil {
			log.Printf("created fallback task %s but could not start it: %v\n", shortID(task.ID), err)
		} else {
			result.Started = true
		}
	}
	return result, nil
}

// ideaTitle makes a card label out of a brief. It counts runes, since an idea
// arrives as whatever the user typed.
func ideaTitle(idea string) string {
	line := strings.TrimSpace(idea)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		return "Untitled idea"
	}
	r := []rune(line)
	if len(r) <= ideaTitleLen {
		return line
	}
	return string(r[:ideaTitleLen-1]) + "…"
}
