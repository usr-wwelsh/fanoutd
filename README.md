# fanoutd

Multi-agent orchestrator with a kanban board, driven by any OpenAI-compatible model provider — hosted or local.

Two binaries in one module:

```
cmd/fanoutd/      server: sqlite, agent loop, embedded UI
cmd/fanout/       client: net/http and stdout
internal/client/  shared HTTP client
```

`fanout` does not link the sqlite driver, so it physically cannot open a local
database — it reaches a board only over HTTP, from anywhere.

## How it works

Columns organize tasks; they never start or stop anything on their own. The agent
runs only when you press **Start Agent**.

1. **Ideas** — Create tasks with a goal.
2. **To-Do** — Where a task lands when you start it. The agent plans step by step
   via the configured provider, writes files into its workspace, and records a full trace.
3. **Review** — Only with `FANOUT_REVIEW=1`. Where a finished run waits for a
   second agent to check it. Nothing is ever filed here with review off.
4. **Finished** — Where a task lands when the agent reports the goal met, with a
   summary. Dragging a running task out of To-Do stops it.

Separate from the column, every task has a status: `idle`, `running`, `done`,
`stopped`, or `error`. Starting a stopped or failed task resumes it with its
existing trace. A run only exists in the server's memory; on shutdown, cancelled
runs get 30 seconds to record where they got to, and anything still `running` at
the next startup is reclaimed as `stopped`.

The agent stops itself on **repetition** (the same action or tool call three
times without progress), **invalid output** (three consecutive unparseable
responses), or the **step limit** (20). The workspace then decides how the run is
filed: files present means Finished, with a summary naming them and saying the
agent never called `finish` — a model that writes a working file and then loops
re-reading it is the common case, and that deliverable is not a failure. An empty
workspace ends in `error`.

## Breaking an idea into subtasks

`fanout breakdown "<idea>" --start --watch`, or **Break Down** on the board.

One model call turns the idea into subtasks, each with a goal, the paths it
`writes` and `reads`, and the `criteria` its output will be held to. Those two
lists are the whole design:

- **Ownership.** A path has exactly one writer, enforced by a primary key. A
  second subtask that tries to write it gets a tool error naming the owner.
- **Ordering.** Dependencies are *derived*, not declared: if B reads a path A
  writes, B waits for A. A model asked for a dependency list produces something
  plausible and wrong; asked which files a subtask touches, it is answering about
  the work itself.
- **The contract.** The same call returns the interface the parts meet at, and it
  goes to every subtask verbatim. The agents never speak to each other, so
  whatever is not settled here is settled N times over.
- **The criteria.** Two to four checkable statements per subtask about what its
  output must do, settled before any work starts. The agent is shown them, and
  review checks against them. A plan whose subtasks carry none is re-planned: a
  criterion written afterwards is written by whoever already knows what was built.

Subtasks share one workspace, so there is no merge step. A file every subtask
would need goes to a final integration subtask that owns it, reads its siblings'
outputs, and is briefed to run the whole thing rather than add anything of its
own. A plan where two subtasks write one path is rejected in memory and re-planned
once; still conflicting, unreachable, or cyclic, and the idea becomes one ordinary
task. Nothing is created until a plan passes.

Subtasks run rolling-parallel — each starts the moment its dependencies finish and
a slot is free, capped by `FANOUT_MAX_PARALLEL`. A failed subtask cascades:
everything downstream is skipped rather than run against files never written.

On the board a breakdown is one card with a single badge for the whole plan.
Dragging it files every subtask at once; deleting it removes all of them and the
shared workspace. `fanout plan <group>` shows the waves and each subtask's state.

## Seeding a workspace

`--seed` puts material in the workspace before anything runs. It takes a file or
directory and is repeatable:

```bash
fanout add "port it" --goal "port old.py to Go" --seed old.py --start
fanout breakdown "split this spec into pages" --seed spec.md --seed assets/ --start
```

A file lands under its own name, a directory under its own name as a prefix.
Dotted names are skipped at every level, so `.git` and `.env` are never handed to
an agent. The CLI sends contents with the request, because the board may be on
another machine — text only, 256 KB per file and 2 MB in total. For a breakdown
the seed is shown to the planner too; seeded files are unowned, and a subtask may
claim one in `writes` if its job is to replace it.

## Agent tools

The workspace is `$FANOUT_DATA_DIR/output/<workspace-id>/`.

| Tool | Arguments | Effect |
|------|-----------|--------|
| `write_file` | `path`, `content` | Create or overwrite a file |
| `read_file` | `path`, `offset` | Read a file, 6 KB per call, paged by `offset` |
| `edit_file` | `path`, `old`, `new` | Replace the first occurrence of `old` |
| `delete_file` | `path` | Delete a file |
| `list_files` | — | List the workspace |
| `run_command` | `command` | Shell command in the workspace (needs `FANOUT_SHELL=1`) |
| `finish` | `summary` | End the run |

A reviewer gets `read_file`, `list_files` and `run_command` only, with `pass` and
`reject` in place of `finish`.

Paths are relative and resolved inside the workspace; `..` escapes are rejected,
and an absolute path is folded back onto the root — the model is shown its
workspace directory in full and routinely answers with one.

The trace is replayed into every step's prompt under a total byte budget: the
newest steps go back whole and older ones keep their shape but lose their bulk, so
a long run's prompt stops growing with its length. What is elided is on disk.

Repetition ends a run only when nothing moved: a call that does not change the
workspace is counted against the state it ran on, so reading a file back after
editing it is progress rather than a loop.

Files appear in the task detail panel, each marked with whether this task wrote it
when the workspace is shared. **Open** serves a workspace at `/preview/<task-id>/`
so a page loads with the scripts and styles beside it; the file name opens raw
bytes.

## Shell commands

Off by default. `FANOUT_SHELL=1` gives agents `run_command`, so they can compile
and test what they write instead of only writing it.

The command line is never parsed, escaped, or matched against an allowlist. It
goes straight to `/bin/sh` inside a
[bubblewrap](https://github.com/containers/bubblewrap) jail, because the jail is
the security boundary and there is nothing inside it to escape to. That is also
what keeps fanoutd language-agnostic: whatever toolchain the host has under `/usr`
is what an agent can run.

**bubblewrap is mandatory.** At startup the sandbox is probed by running a real
command through the real jail — not by looking for the binary, since an
unprivileged user namespace can be missing or blocked. If the probe fails,
`run_command` is never advertised. There is no unsandboxed fallback.

Inside the jail: no network, nothing writable but the workspace and `/build`,
`--clearenv` so `FANOUT_API_KEY` is never in scope, resource limits from a
transient systemd user scope, a wall-clock timeout that starts after a command
acquires a slot, and output capped head-and-tail since results are replayed into
the prompt.

The workspace is mounted at **the same absolute path it has on the host**, and
every command starts there — the model is told that path in its prompt, so a shell
that disagreed would make the prompt true for the file tools and false for this
one.

Build artifacts stay out of the workspace: each task gets a private `/build`
(`CARGO_TARGET_DIR`, `GOPATH`, `HOME`) and all tasks share `/cache` (`GOCACHE`,
`GOMODCACHE`, `CARGO_HOME`, npm). Private build directories are not just tidiness
— cargo locks its target directory, so a shared one would serialize parallel
subtasks into what looks like a hang. A task's build directory is removed with the
task, and orphans are reaped at startup.

Shell writes are reconciled against the claim table afterwards, since a command
bypasses `write_file` entirely: paths the task created become its own, and paths
another task owns are reported back in the same shape as a refused write.

`FANOUT_SHELL_ROBIND` mounts extra host paths read-only for toolchains outside
`/usr` (rustup, nvm, pyenv, `go install`); any entry with a `bin/` subdirectory
joins `PATH`. Empty by default, because binding a home directory would hand every
agent your ssh keys.

See [.env.example](.env.example) for every knob.

## Review

Off by default. `FANOUT_REVIEW=1` puts a second agent between a run ending and
its work being filed. It is the same loop with the write tools taken away and
`finish` replaced by `pass` / `reject`.

What matters is what the reviewer is *not* given. It never sees the author's
trace — only the goal, the criteria settled before any work started, the author's
summary as a claim to check, and the files. A reviewer replaying the author's
reasoning inherits the author's reasons for the shortcuts. Set
`FANOUT_REVIEW_MODEL` to something other than the author's model too; a model
reviewing its own output agrees with it.

It is worth much more with `FANOUT_SHELL=1`, since the reviewer can then execute
what it is judging rather than only read it. Advertising a narrower tool set is
not enough on its own — a `write_file` from a reviewer is refused at the point of
execution, not merely left off the menu.

**Pass** files every task it covers as Finished, keeping both the author's
summary and the verdict. **Reject** leaves the reviewed work in Review and opens
a *rework* task: same workspace, the findings as its goal, the same criteria, and
it starts immediately. That happens at most twice — past `maxReviewRounds` the
work stops going round and is parked with an `error` status, which is what
`fanout blocked` lists.

A reviewer gets a step budget that grows with what its verdict covers, since a
breakdown's verdict is over every subtask's files and criteria at once. When it
runs out — or goes round the same call, which for a reviewer converges on nothing
since it cannot change the work — it gets one more turn with the reading tools
taken away and the question put directly: on what you have seen, pass or reject.
A criterion it never reached is one it could not confirm, and it is told to say
so in the findings. Only a reviewer that will not answer even then has reached no
verdict; that changes nothing and parks the work with an `error` for a person,
which is the worst of the three outcomes and no longer the one a reviewer falls
into by reading too much.

A run that hit its own step limit is filed as done and reviewed like the rest —
it is the run most likely to be half-built, which is an argument for checking it
rather than for filing it unchecked. The reviewer is told outright which parts
those were, rather than being left to notice it in the summary.

Solo tasks are reviewed one at a time. **A breakdown is reviewed whole**, once
every subtask is in and only if every one of them finished: sending one subtask
back would invalidate every sibling that already read its output, and the claims
arbitrate concurrent writes rather than stale reads. Its rework task gets the
shared workspace but no group, so it is free to fix whichever file the findings
name.

Whichever run completes the group asks for that verdict, whether it was the
schedule or a single subtask somebody restarted by hand after a stop. The group
is claimed while the review runs, so a start arriving underneath it is refused
rather than re-running what the reviewer is reading.

Starting a breakdown that is part-way through **resumes** it: subtasks already
awaiting review or filed as done are left alone and count as satisfied for
whatever depends on them, and only what did not finish is run. Dragging a plan
back to To-Do is how you ask for the whole thing again.

A rework's own verdict settles the work it repaired. Rejected work stays in
Review while its rework runs, and nothing else ever looks at those rows again —
so a rework that passes files them with itself, and one that is rejected again
carries them round with it until the round limit parks the lot. The reviewer is
held to the original goal rather than to the findings, or a rework passes having
fixed the one line named and broken everything around it.

Verdicts are recorded on the task's own trace, and skipped when that task's
transcript is replayed — an author handed a critique of itself as a turn it made
will argue with it.

A verdict is delivered by the goroutine that ran the task, so a restart between a
run settling and its review finishing would strand the work in Review: done,
unjudged, and not what `fanout blocked` lists. The server sweeps for that at
startup and delivers the verdicts the previous process owed, one at a time, in
the background — a breakdown counting as one and a rework as covering what it
repairs. With `FANOUT_REVIEW` since turned off, it says how much is sitting there
rather than judging it.

## Setup

```bash
go mod download
bun install --cwd frontend
export FANOUT_API_KEY=sk-or-v1-...
```

Development, in two terminals — or `./run.sh` for both:

```bash
bun run --cwd frontend dev
go run ./cmd/fanoutd
```

Production. The frontend is embedded, so build it *before* the server: `go build`
bakes in whatever is in `cmd/fanoutd/dist/` at that moment.

```bash
bun run --cwd frontend build
go build -o fanoutd ./cmd/fanoutd
go build -o fanout ./cmd/fanout
```

The server is a single self-contained file; copy it anywhere and run it. `fanout`
is a separate ~6 MB binary that belongs on whatever machine you type from.

## Configuration

| Env Variable | Default | Description |
|-------------|---------|-------------|
| `FANOUT_PROVIDER` | `openrouter` | Which endpoint to talk to — see below |
| `FANOUT_API_KEY` | *(required)* | Key for that provider; local ones need none |
| `FANOUT_MODEL` | *(the provider's, if it has one)* | Model id |
| `FANOUT_BASE_URL` | *(the provider's)* | Override the endpoint |
| `PORT` | `8080` | API server port |
| `FANOUT_TOKEN` | *(empty)* | Gates the API. Empty leaves the server open |
| `FANOUT_MAX_PARALLEL` | `3` | Concurrent subtasks within one breakdown |
| `FANOUT_DATA_DIR` | `$XDG_DATA_HOME/fanoutd` | Database and workspaces |
| `DATABASE_PATH` | `fanoutd.db` | Relative to the data directory |
| `OUTPUT_DIR` | `output` | Relative to the data directory |
| `FANOUT_SHELL` | `0` | Enable `run_command`; the rest of the shell knobs are in `.env.example` |
| `FANOUT_REVIEW` | `0` | Send finished runs to a reviewing agent before Finished |
| `FANOUT_REVIEW_MODEL` | *(the task's own)* | Model the reviewer runs on; pick a different one |
| `FANOUT_ENV_FILE` | *(below)* | Override the settings file location |

The `OPENROUTER_*` names predate there being a choice, and still work: an
existing env file needs no edit.

Nothing resolves against the working directory, so the server behaves identically
started from the repo, a systemd unit, or `/`. Settings are read from the first
file that exists: `FANOUT_ENV_FILE`, then `./.env`, then
`$XDG_CONFIG_HOME/fanoutd/env`; exported variables win over the file. Copy
`.env.example` to `.env` to start, or install it at the XDG path for a deployed
server.

**Providers.** Every one of these speaks OpenAI `chat/completions`, which is why
the list can be this long without a plugin system behind it — a provider is a
base URL and a key, not an integration.

| | |
|---|---|
| Hosted | `openrouter` · `openai` · `anthropic` · `gemini` · `groq` · `deepseek` · `mistral` · `xai` · `together` · `fireworks` · `cerebras` |
| Local | `ollama` · `llamacpp` · `vllm` · `lmstudio` |
| Anything else | `custom`, with `FANOUT_BASE_URL` |

```bash
FANOUT_PROVIDER=ollama FANOUT_MODEL=qwen3 go run ./cmd/fanoutd
```

Local providers need no key, carry their upstream default port, and are the only
configuration that owes nobody a network — which, with `FANOUT_SHELL=1` and its
jail, is a board that plans, writes, runs and reviews entirely on one machine.
`FANOUT_BASE_URL` is what points any of them at a different host or port.

A provider that cannot work is a startup error naming what is missing, not a run
that discovers it six steps in and files the result as a failed task. Vendors
without one obvious model require `FANOUT_MODEL`; only `openrouter` ships a
default, because it has a free tier to default to.

**The model picker degrades with the provider.** OpenRouter publishes pricing,
context length and per-model parameter support, so the picker groups free from
paid and marks which models call tools. The OpenAI `/models` schema specifies
none of that, and almost nobody adds it: everyone else returns ids. So the
response says which kind of catalog it is, and the picker shows a plain list —
or a text field, for a local server that implements no `/models` at all.

The alternative was reading the missing fields off a bare response anyway, where
they parse cleanly as zero and mean every model is free and none of them can
call tools. Both false. `fanout models` says the same thing in one line.

`anthropic` and `gemini` are reached through their OpenAI-compatible endpoints.
Their native APIs shape tool calls differently and would be a second
implementation of one Go interface — the seam is there, and nothing else in the
codebase would notice.

**Rate limits.** The default model is free, and the free tier is capped per minute
across the whole key — which three concurrent subtasks reach easily. A 429 is
waited out rather than failed: five retries, sleeping until the reset OpenRouter
names in `X-RateLimit-Reset`, plus a random spread so subtasks refused together do
not collide again. Lower `FANOUT_MAX_PARALLEL` if you see it often; a paid model
is the other way out.

**Authentication.** `FANOUT_TOKEN` gates every `/api` route except `/api/health`
and the auth endpoints. One secret, two transports: `Authorization: Bearer
<token>` for scripts and the CLI, and a 30-day `HttpOnly` session cookie for the
UI, which is what makes the board usable from a phone. Empty keeps local
development frictionless, but this server runs an agent that writes files and
spends credits — set a token before exposing it beyond localhost.

## API

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/tasks` | List all tasks |
| POST | `/api/tasks` | Create task (starts in Ideas); optional `seed: [{path, content}]` |
| GET · PUT · DELETE | `/api/tasks/:id` | Get task + trace, update, delete |
| POST | `/api/tasks/:id/move` | Move column (stops the agent if moved out of To-Do) |
| POST | `/api/tasks/:id/start` | Start (or resume) the agent loop; 409 if already running |
| POST | `/api/tasks/:id/stop` | Cancel the running agent loop |
| GET | `/api/tasks/:id/trace` | Full trace breakdown |
| GET | `/api/tasks/:id/status` | Task status, error, and whether a run is live |
| GET | `/api/tasks/:id/files` | Files the agent has written |
| GET | `/api/tasks/:id/raw?path=` | One workspace file, served inline |
| POST | `/api/tasks/:id/continue` | New goal against the same workspace |
| POST | `/api/tasks/:id/retry` | Same brief, clean workspace |
| POST | `/api/breakdown` | Split an idea into subtasks; blocks on the model; optional `seed` |
| GET | `/api/groups/:id/plan` | The resolved waves and every subtask's state |
| POST | `/api/groups/:id/start` · `/stop` | Run the schedule, or cancel it and everything under it |
| GET | `/api/models` · `/api/health` | Accepted models and its default; health check |
| GET · POST | `/api/auth` · `/login` · `/logout` | Token state, session cookie in, session cookie out |
| GET | `/preview/:id/*` | A task's workspace served as a site — same gate as `/api` |

## The `fanout` CLI

```bash
fanout add "Tetris clone" --goal "build a playable tetris in one html file" --start --watch
fanout ls
fanout show c762
fanout cat c762 tetris.html
```

```
c762903  Tetris clone          todo      running   step 7      write_file wrote tetris.html
88d9af4  Research digest MVP   finished  done      12 steps    3 files
```

| Command | Notes |
|---|---|
| `add <title> [--goal ...] [--desc ...] [--seed path] [--model ...] [--start] [--watch]` | `--goal -` reads the goal from stdin |
| `breakdown "<idea>" [--seed path] [--model ...] [--start] [--watch]` | split it into subtasks and run them; `-` reads stdin |
| `plan <group> [--start] [--watch] [--json]` | the wave plan of a breakdown, and its subtasks |
| `ls [--col todo] [--status running] [--json] [--plain]` | the table above |
| `blocked [--resume] [--all] [--json]` | runs that stopped short, and why |
| `show <id> [--last N] [--json]` | task, files, and its last few steps |
| `watch <id> [--interval 2s]` | prints steps as they land, exits when the run ends |
| `trace <id> [--last N] [--full] [--json]` | truncated by default |
| `start <id> [--watch]` / `stop <id>` | the agent loop |
| `mv <id> <column>` / `rm <id> [--keep-files]` | organize and delete |
| `files <id> [--all] [--abs]` / `cat <id> <path>` | workspace output |
| `continue <id> --goal ...` | new goal, same workspace |
| `retry <id> [--model ...]` | same brief, clean workspace |
| `models` | what this server accepts for `--model` |

**Blocked runs.** Every guard ends a run with the reason already on the task, so
"what needs a nudge?" is one request:

```
$ fanout blocked
31fcf15  Game entry point and UI  group f04073e  repeated the same action 5 times
acebc62  test (2)                 -              interrupted by a server restart

2 blocked tasks — resume with `fanout blocked --resume`
```

`--resume` starts every task listed from its existing trace and does not stop at
the first one the server refuses. The default scope is To-Do and Review — the two
columns where something is waiting on you — and Ideas and Finished need `--all`.

**Id prefixes.** Any unambiguous prefix works, and so does part of a title —
`fanout show c762`, `fanout show Tetris`. Ambiguity is an error listing the
candidates, never a guess. Group ids resolve the same way.

**Exit codes.** `0` succeeded, `1` the command failed, `2` the command worked and
the *task* ended in `error` — which is what makes `fanout watch` composable:

```bash
fanout start c762 --watch && ./deploy.sh
```

Server precedence: `--server` → `FANOUT_URL` → `$XDG_CONFIG_HOME/fanoutd/config.toml`
→ `http://localhost:8080`. Same for `--token` / `FANOUT_TOKEN`. Nothing is derived
from the working directory or the binary's location.

```toml
url = "https://board.example"
token = "..."
```

## Roadmap

See [ROADMAP.md](ROADMAP.md) — repo seeding is what remains.
