---
name: fanout
description: Offload work to a fanoutd board with the `fanout` CLI — break an idea into parallel subtasks, run them, watch them to completion, and collect the files they produce. Use when the user says "fan this out", "break this down with fanout", "run it on the board", "hand this to the agent", asks about a task/group/plan on their board, or asks to check on something already running there.
---

# fanout

`fanout` is a thin HTTP client for a `fanoutd` board. It never touches a local
database — every command is a call to a server that may be on this machine or
somewhere else. Nothing resolves against the working directory, so the same
command works from any repo.

The unit of work is a **task** (one goal, one sandboxed workspace, one agent
loop) or a **group** (an idea split into subtasks that share a workspace and run
rolling-parallel).

## Preflight

Run once before anything else:

```bash
fanout ls --plain
```

Three failure modes, each with a fixed response:

| Output | Meaning | Do |
|---|---|---|
| `connection refused` | no server at the resolved URL | tell the user; do not start one unless asked |
| `requires a token` | server gates `/api` | see below |
| a table (or nothing) | ready | proceed |

For the token, do **not** read it out of a repo `.env` and paste it into a
command line — it lands in shell history and in the transcript. Ask the user to
create the client config instead:

```toml
# ~/.config/fanoutd/config.toml
url = "https://board.example"
token = "..."
```

`--server` / `--token` flags beat `$FANOUT_URL` / `$FANOUT_TOKEN`, which beat
that file, which beats `http://localhost:8080`.

## Breaking an idea down

```bash
fanout breakdown "<idea>" --start
```

One model call splits the idea into subtasks, each declaring the paths it
`writes` and the paths it `reads`. Ordering is *derived* from those lists: if B
reads a path A writes, B waits for A. Every path has exactly one writer, enforced
by the server — a subtask that writes someone else's file gets a tool error
naming the owner.

**Write the idea for that mechanism.** It is being handed to a model that has to
name files, so:

- Say what artifacts exist. "a CLI with a scanner, a renderer, tests, and a
  README" splits cleanly; "make it good" does not.
- Name the language and the shape of the deliverable. The agent starts from an
  empty workspace and has no view of your repo.
- Anything every subtask would touch — an index, a manifest, a shared
  stylesheet — should be described as a final integration step, so it lands as
  one subtask that owns the file and reads its siblings' output.
- One coherent deliverable per breakdown. Two unrelated ideas produce a plan with
  no edges and no reason to be a group.

`breakdown` blocks on the model — allow ~30s. If the plan can't be made
conflict-free after one re-plan, or is cyclic, the idea is created as a single
ordinary task carrying the original wording. That is a *successful* command with
a different result: check the output before assuming you got a group.

For work that is already one job, skip the split:

```bash
fanout add "<title>" --goal "<what done looks like>" --start
```

`--goal -` reads the goal from stdin, which is the right way to pass anything
long or multi-line.

## Watching

`--watch` blocks until the run ends. Runs take minutes, so **never** put a
watch in a foreground Bash call — it will hit the tool timeout and you will
lose the exit code, which is the only part that matters. Two correct patterns:

**Background the watch** (preferred — you get notified on exit):

```bash
fanout breakdown "<idea>" --start --watch    # run_in_background: true
```

**Or poll.** Cheap, bounded, and safe to interleave with other work:

```bash
fanout plan <group>     # every subtask, by wave, with state
fanout show <id>        # one task: files, status, last few steps
```

Poll on the order of tens of seconds. A step is an LLM call; nothing changes in
two seconds that you needed to see.

Exit codes are the contract:

- `0` — command worked, task finished clean
- `1` — the command itself failed (bad id, no server, auth)
- `2` — the command worked and the **task** ended in `error`

So `fanout start c762 --watch && ./deploy.sh` is meaningful, and a `2` from a
backgrounded watch means read the trace, not retry blindly.

## Reading results

```bash
fanout show <id>            # status, files, last steps
fanout files <id>           # what it produced
fanout cat <id> <path>      # one file to stdout
fanout trace <id> --last 5  # recent steps, truncated
```

Prefer `show` and `trace --last N`. `trace --full` dumps every prompt and
response verbatim — it is for a human debugging a bad run, and it will bury your
context window. `--json` on `ls`, `show`, `trace`, and `plan` when you need to
parse rather than read.

Ids resolve by any unambiguous prefix or by part of the title: `fanout show
c762` and `fanout show Tetris` both work, and group ids resolve the same way.
Ambiguity is an error listing candidates — it never guesses, so widen the prefix
rather than retrying.

To bring output into the repo, `fanout cat` it and write the file yourself.
Never copy out of `output/` by path: the workspace belongs to the server, which
may not be this machine.

## When a run goes wrong

The agent stops itself on repetition (same call three times), three consecutive
invalid responses, or 20 steps. A stopped run keeps its trace.

| Situation | Command |
|---|---|
| stopped or errored, work is salvageable | `fanout start <id>` — resumes from the existing trace |
| the brief was fine, the run was unlucky | `fanout retry <id> [--model ...]` — same brief, clean workspace |
| it produced something, now needs more | `fanout continue <id> --goal "..."` — new goal, same workspace |
| it's running and shouldn't be | `fanout stop <id>` (a group id stops the whole schedule) |

A failed subtask cascades: everything downstream is skipped rather than run
against files that were never written. So fix the failing subtask and restart
it — the schedule picks the rest back up.

`fanout retry` discards the workspace. Confirm with the user before retrying
anything whose files they may want, and `fanout files` first if unsure.

Two more things that stop a run as a side effect: moving a running task out of
`todo` (`fanout mv`), and `fanout rm`, which deletes the workspace too unless
you pass `--keep-files`. `rm` on a group card removes every subtask under it.
Both are destructive to someone else's board — ask first.

## Don't

- Don't start a task the user only asked you to describe. `breakdown` without
  `--start` plans it and leaves it in Ideas.
- Don't run `fanoutd` or reach for the database. This skill is the client half;
  the server's lifecycle is the user's.
- Don't treat a fallback single task as a failed breakdown, or a `2` exit as a
  broken command.
