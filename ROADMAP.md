# Roadmap

Steps 1 (portability + auth), 2 (the `fanout` CLI) and 4 (idea breakdown) are
done. Step 3, repo seeding, is what remains.

Each step is independently shippable. Step 4 was deliberately last as the only
one with unresolved design questions; those were settled, and it overtook step 3
on the way past.

---

## Step 2 — `fanout` CLI client — **done**

Shipped as described: a second binary in the same module that talks to the
server over HTTP only, documented in the README. Three notes on what the plan
got wrong or left out.

**Size.** `cmd/fanout` links no sqlite, which was the point and holds. The 3 MB
estimate did not: it is 9.4 MB, or 6.4 MB stripped. `net/http` and `crypto/tls`
are the floor, and neither is optional for reaching a remote board over HTTPS.

**`step 7/20`.** The step cap lives in `agent.Loop` and is not exposed over
HTTP, so the CLI cannot honestly print a denominator. It prints `step 7`. If
the cap ever becomes worth showing, `/api/tasks/:id/status` is where it belongs.

**Flag order.** Go's `flag` package stops parsing at the first non-flag
argument, which silently ignored `fanout trace c762 --full` — the id-first form
everyone actually types. `permute` in `cmd/fanout/main.go` reorders arguments before
`Parse`, consulting the flag set so a bool flag does not swallow the positional
after it. This was a real bug caught by the tests, not a theoretical one.

Two additions beyond the plan, both the same idea as prefix resolution:
`fanout cat <id> board.js` resolves an unambiguous basename against the file list,
and `--watch` on `add`, `start`, `continue`, and `retry` avoids a second
command to follow a run just started.

---

## Step 3 — Repo seeding

`fanout add "explain this codebase" --repo github.com/usr-wwelsh/turbolab --start`

Shallow-clone a repository into the task workspace before step 0, so the agent
can walk it with the tools it already has.

### Clone strategy

```
git clone --depth 1 --single-branch <url> <workspace>/repo
```

Not `--bare`: that skips the working tree but keeps the **full** object
database, so it saves almost nothing on disk and leaves `list_files` and
`read_file` staring at pack files. The agent cannot read source from a bare
clone.

Not `--filter=blob:none`: blobless partial clones fetch file contents from the
remote on demand, which means a network round trip every time the agent reads a
file. That breaks offline-first.

For read-only "explain this" tasks, deleting `.git` after the clone saves more
than any flag. Keep it only once the agent needs to produce diffs.

### CLI work

`fanout files <id> --abs` prints `size  mtime  path`, so it does not compose:
`$(fanout files X --abs | tail -1)` hands you the whole row rather than a path.
Give `--abs` a bare-path mode — either `--plain` alongside it, or one path per
line whenever stdout is not a terminal — so the file list can be piped into
`xargs` and `$(...)`. Small, but it is the difference between an agent being
able to chain `fanout` calls and having to hand-parse columns.

### Server work

- `POST /api/tasks/:id/repo` — clone into the workspace. Reject while the task
  is running. Cap clone size and wall-clock time; a hung `git` must not wedge a
  task.
- `POST /api/tasks/:id/files` — multipart upload for the smaller case (a spec, a
  schema, a few notes). Resolve through the existing `Workspace.ResolvePath`,
  which already rejects `..` and absolute paths.
- Both refuse to run once a trace exists — seeding is a pre-step-0 operation.

### Agent work

Almost none. `buildMessages` already announces pre-existing workspace files to
the model when the trace is empty:

```go
if len(existing) > 0 && len(trace) == 0 {
    intro.WriteString("\nThis workspace already holds work from an earlier task:\n")
```

One wording fix: that sentence is wrong for seeded input, which is provided
context rather than prior output. The two cases need distinct phrasing, and a
cloned repo wants a directory summary rather than a flat file list — a
thousand-entry enumeration would blow the intro budget on its own.

---

## Step 4 — Idea breakdown into subtasks — **done**

Split one idea into several subtasks and run them.

### What the question turned out to be

The plan framed this as a choice between a workspace per subtask with a merge,
and one shared workspace run serially. Both were answers to the wrong question.
Two problems were being treated as one:

- **Ordering** — "write the tests" and "write the thing they test" are not
  parallel. That is a dependency graph.
- **Collision** — two agents editing `board.js`. Ordering does *not* fix this.
  Two subtasks with no dependency between them can still both want that file.

Collision is fixed by making a path have exactly one writer, which is a
uniqueness constraint, not a lock and not a merge. That is one primary key:

```sql
CREATE TABLE task_writes (
  workspace_id TEXT NOT NULL,
  path         TEXT NOT NULL,
  task_id      TEXT NOT NULL,
  PRIMARY KEY (workspace_id, path)
);
```

With that in place the shared workspace gets real isolation and still needs no
merge step. Both branches of the original choice are obsolete.

### Claims — **done**

`internal/store/claims.go` and the ownership half of `internal/agent/tools.go`.

`write_file`, `edit_file` and `delete_file` resolve through `resolveOwned`,
which claims the path first and returns a plain tool error naming the owner —
and the paths this task *does* own, which is the only actionable part — when
another subtask holds it. Reads and `list_files` are unarbitrated: consuming a
sibling's output is the normal case.

First writer wins on an unclaimed path. A breakdown cannot predict every file
its subtasks will create, so the plan is mandatory for the paths it names and
advisory for the rest.

Claims key on the path relative to the workspace root, not on what the model
typed. `board.js`, `./board.js` and the absolute form are one claim; keying on
the raw string was the obvious way to walk straight past the constraint.

Arbitration is enabled by `Task.GroupID`, not by workspace sharing. A task that
*continues* an earlier one also shares a workspace and must stay free to edit
everything it inherited — keying off the workspace would have broken it.

### Scheduling — **done**

`internal/agent/schedule.go`.

Dependencies are **derived, not declared**: if B reads a path A writes, B
depends on A. A model asked for a dependency list produces something plausible
and wrong; the same model asked which files a subtask reads and writes is
answering about the work itself. It also collapses two things to get right into
one — a wrong file partition makes the claims and the schedule wrong in the
same direction, which is discoverable, rather than in different directions.

`PlanGroup` resolves the graph without running anything, so a cycle surfaces
before the first token is spent. `topoWaves` is Kahn's algorithm and doubles as
the plan a user can be shown.

Execution is rolling rather than wave-lockstep: a subtask starts the moment its
own dependencies finish and a slot is free. Model latency varies by an order of
magnitude between subtasks, so lockstep waves would idle slots behind the
slowest sibling in each one.

A failed subtask cascades. Everything downstream is marked skipped rather than
run against files that were never written, and the sweep follows the waves in
order — a skip is itself a failure its own dependents must inherit, so visiting
a task before its two-levels-up blocker was marked would strand it.

`FANOUT_MAX_PARALLEL` (default 3) caps concurrency. The limit that binds is
the provider's, not the machine's.

### One bug this exposed

Parallel subtasks are the first thing in this codebase to write the database
concurrently, and `sql.Open` had no busy timeout — the second writer failed
instantly with `SQLITE_BUSY` rather than waiting. `store.dsn` now sets
`busy_timeout` and WAL. It has to ride on the DSN because `busy_timeout` is
per-connection and `database/sql` pools several; a one-off `Exec` would set it
on whichever connection happened to serve that call.

This was latent before, not introduced here. Concurrent runs were rare enough
to never hit it.

### The breakdown pass — **done**

`internal/agent/breakdown.go`. One call returning, per subtask, a title, a goal,
and the paths it `writes` and `reads`; the reply is read with the same
`extractJSON` scan the step parser uses, generalized to take the envelope key it
should recognise, because a breakdown arrives fenced and narrated exactly like
everything else a model sends.

The prediction that the tuning time would go into the prompt rather than the
scheduler held. Four things in it earned their place:

- **The file lists come first.** The prompt says to decide them before writing
  the goals, because a model that writes the goals first partitions the files to
  fit prose it has already committed to.
- **Ordering has exactly one expression.** Naming a sibling's output in `reads`
  is the only way to order anything — no "after step 2", no numbering, no
  dependency sentences in a goal. Left unsaid, the model does all three and
  none of them are read by anything.
- **Every goal must stand alone.** A subtask sees its own goal and nothing else.
- **The idea reaches each subtask as `Details:`, and says so.** A subtask that
  can see the whole idea tries to build the whole idea and then loses every file
  it does not own. The wording names it as one part and says siblings own the
  rest, which turns context into a boundary rather than an invitation.

### Validation, and what it is allowed to cost

Nothing is created until a plan passes. Two subtasks writing one path, a subtask
that writes nothing, a plan of one, a cycle — all are found in memory, and the
first failure is fed back verbatim in a single re-plan. The retry also forbids
the cheap fix: a model told only "two subtasks write board.js" resolves it by
deleting one of them, which is a conflict-free plan that discards the work.

Two additions to what the plan called for:

**Path normalization at parse time.** Claims key on the path relative to the
workspace root, so `board.js`, `./board.js` and `/board.js` are one claim. The
validator has to reduce paths the same way or a plan with both forms passes
in memory and collides in the database — where the only remedy left is the
fallback, having already spent both model calls.

**Cycles are caught before creation, not after.** `PlanGroup` would find one,
but by then the tasks exist and the only move is to unwind and fall back.
Running `deriveDeps` and `topoWaves` over the proposed partition first makes a
cycle re-plannable, which is the same class of mistake as a contested path and
deserves the same second chance. `topoWaves` now returns a typed `cycleError`
carrying the unorderable ids, so the pre-creation check can report the titles a
user typed rather than positions they have never seen.

The fallback is one ordinary task with the original idea as its goal — reached
by a plan that stays broken, an unreachable provider, and anything that fails
during creation. `buildGroup` deletes everything it made before returning, so
the floor never starts from a half-built group.

### Surface

`POST /api/breakdown`, `GET /api/groups/:id/plan`, `POST /api/groups/:id/start`
and `/stop`. Start was not in the plan; without it a group created but not run
is unreachable, which is a dead end the other three cannot open.

`/api/breakdown` is the first endpoint that blocks on a model rather than on the
database, so it has a budget of its own — five minutes, which is also the
server's write deadline and the CLI's timeout for that one call. A shorter
deadline does not cancel the work: the handler goes on to build the group, and
the caller is left with an error and a board that grew a group anyway.

`fanout breakdown "<idea>" [--start] [--watch]` and `fanout plan <group>` carry it
in the CLI, with group ids resolving by prefix like task ids — a group has no
row of its own, so the candidates come from the task list, and a group is also
named by any of its subtasks' titles. `fanout stop <id>` falls through to groups
when the id matches no task, which costs nothing: a group id shares no prefix
with its subtasks. `fanout show` prints the group a subtask belongs to, which is
the only place a group id surfaces.

The button opens a dialog that stays open after it submits — the wave plan and
the subtasks running under it *are* the result, and a modal that closed on
success would hide them.

### Files that cannot be partitioned

A router table, a shared index, a `package.json` — something every subtask must
touch. Do not build a merge engine for it. Make it a node: an integration
subtask that depends on all its siblings and owns exactly the shared paths. It
serializes last through the same topological sort, in the same graph, with no
special-case code. That is the merge step, and it is only scheduling.

This is a rule in the breakdown prompt and nothing else in the codebase knows
about it. The scheduler cannot tell an integration node from any other subtask,
which is the point.

---

## Tests

Not a phase — folded into each step as it lands.

The response parser in `internal/agent/loop.go` — `parseResponse`,
`extractJSON`, `stripFences`, `matchBrace` — was named as the highest-value
target and is now covered (`internal/agent/loop_test.go`), along with the CLI
(`cmd/fanout/*_test.go`, driving whole commands against a fake board so the exit
codes are part of the test rather than an implementation detail).

Streaming is covered in `internal/openrouter/stream_test.go`. Completions are
requested with `stream: true` and consumed frame by frame, which replaces the
120s total `http.Client` timeout with a 90s *idle* timeout. The distinction is
the point: a total deadline cannot tell a model steadily writing a 20 KB file
from a wedged socket, so raising it to fit the first makes the second take that
much longer to detect. Silence is the honest signal. The tests cover tool-call
fragment reassembly — arguments arrive as a string split across frames and keyed
by index, which is where a naive accumulator produces JSON that will not parse.

The `concede` path is covered in `internal/agent/concede_test.go`. The three
self-imposed guards used to end in `error` unconditionally, which filed a run
that had already written a working file as a failure because the model never
called `finish`. They now judge the run by its workspace; an empty one still
errors.

Shutdown is covered as of the rename: `Loop.Shutdown` in
`internal/agent/shutdown_test.go` and `Store.ReclaimRunningTasks` in
`internal/store/sqlite_test.go`. `StopAll` only cancelled contexts and `main`
exited immediately after, so a restart left tasks marked `running` with nothing
alive to correct them — which mattered because "pull the new binary and restart"
is about to become routine.

`Workspace.resolve` is covered as of step 4 (`internal/agent/tools_test.go`),
which was overdue — it is the only thing standing between a model-supplied path
and the rest of the filesystem, and the claim key is derived from it, so a bug
there would defeat ownership as well as containment.

Claims and scheduling are covered in `internal/store/claims_test.go`,
`internal/agent/tools_test.go` and `internal/agent/schedule_test.go`. The
graph and wave logic are pure functions and tested as such.
`internal/agent/schedule_run_test.go` drives whole schedules against an
OpenRouter-shaped `httptest` server, which is what makes dependency order,
observed parallelism, the concurrency cap and the failure cascade assertable
rather than argued — the `SQLITE_BUSY` bug above surfaced there and nowhere
else. Those tests want `-race`; the scheduler is the first genuinely concurrent
thing in the codebase.

The breakdown pass is covered in `internal/agent/breakdown_test.go` — the parser
and the validator, both pure, which is where a bug is cheapest to find and most
expensive to miss. `breakdown_run_test.go` drives whole breakdowns against a
fake that answers the planning call from a queue of canned plans and hands
everything else to the subtask fake, so the retry, the fallback, the unwind and
a breakdown that goes on to run are all end to end. The planning call is told
apart from a step by its system prompt; a nil client would panic the moment a
schedule reached the model.

`internal/server/groups_test.go` pins the group routes: `/api/groups/:id` and
its actions, the 404 for a group that does not exist, and the method checks. It
is the first server test in the codebase — the group endpoints are the first
ones addressed by something that is not a task id.

Still untested, roughly in order of what a bug would cost:

- **`buildMessages`.** Step 3 changes it, so cover it as part of that step.
- **`nextTitle`** in `internal/server/server.go`. Cheap, pure, and it parses
  integers out of user-supplied strings.
