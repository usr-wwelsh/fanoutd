# fanoutd

Multi-agent orchestrator with OpenRouter integration and a kanban board for task management.

Two binaries in one module:

```
cmd/fanoutd/      server: sqlite, agent loop, embedded UI
cmd/fanout/       client: net/http and stdout
internal/client/  shared HTTP client
```

`fanout` does not link the sqlite driver, so it physically cannot open a local
database — it reaches a board only over HTTP, from anywhere.

## How It Works

Columns organize tasks; they never start or stop anything on their own. The agent runs
only when you press **Start Agent**, and stops when you press **Stop Agent**.

1. **Ideas** — Create tasks with a goal.
2. **To-Do** — Where a task lands when you start it. The agent plans step by step via
   OpenRouter, writes files into its workspace, and records a full trace of every call.
3. **Finished** — Where a task lands when the agent reports the goal is met, along with
   a summary. Dragging a running task out of To-Do stops it.

### Task status

Separate from the column, every task has a status: `idle`, `running`, `done`, `stopped`,
or `error`. Starting a stopped or failed task resumes it with its existing trace.

A run only exists in the server's memory. On shutdown the server stops accepting
requests, then gives cancelled runs up to 30 seconds to record where they got to.
Anything still marked `running` at the next startup belonged to a process that is
gone, so it is reclaimed as `stopped` with *interrupted by a server restart* as the
reason — resume it with `fanout start <id>` and it picks up from its existing trace.

### The agent stops itself on

- **Repetition** — the same action or the same tool call three times without progress.
- **Invalid output** — three consecutive responses that are not valid JSON.
- **Step limit** — 20 steps without meeting the goal.

Each of these ends the run rather than letting it spin, and the workspace decides how
it is filed. Files present means the task lands in Finished, with a summary that names
them and says the agent never called `finish` — a model that writes a working file and
then loops re-reading it is the common case, and that deliverable is not a failure. An
empty workspace means the run really did produce nothing, and it ends in `error` with
the reason on the task.

## Breaking an idea into subtasks

`fanout breakdown "<idea>" --start --watch`, or **Break Down** on the board.

One model call turns the idea into subtasks, each with a goal and the paths it
`writes` and `reads`. Those two lists are the whole design:

- **Ownership.** A path has exactly one writer, enforced by a primary key rather
  than a lock. A second subtask that tries to write it gets an ordinary tool
  error naming the owner, and the files it does own.
- **Ordering.** Dependencies are *derived*, not declared: if B reads a path A
  writes, B waits for A. A model asked for a dependency list produces something
  plausible and wrong; the same model asked which files a subtask touches is
  answering about the work itself.
- **The contract.** The same call returns the interface the parts meet at —
  exported functions, their arguments and return shapes, and who owns anything
  two subtasks could each reasonably build. It goes to every subtask verbatim.
  The agents never speak to each other, so whatever is not settled here is
  settled five times over: left to themselves, one subtask builds a renderer and
  a camera, the next builds its own camera, and both meet their goals while the
  result does not run. A plan whose subtasks read each other and carries no
  contract is sent back.

The subtasks share one workspace, so there is no merge step. A file every
subtask would need — an index, a manifest, a shared stylesheet — is not split:
it goes to a final integration subtask that owns it and reads its siblings'
outputs, which serializes it last through the same topological sort. That
subtask is briefed differently from the rest: it is the only one that ever sees
the whole thing assembled, so it is told to run it and reconcile it against the
contract rather than to add anything of its own.

A plan where two subtasks write one path is rejected in memory, and re-planned
once with the conflict named. Still conflicting, or unreachable, or cyclic, and
the idea is created as one ordinary task carrying the original wording — the
floor matters more than the parallelism. Nothing is created until a plan passes,
so a rejected one leaves no rows behind.

Subtasks run rolling-parallel rather than in wave lockstep: each starts the
moment its own dependencies finish and a slot is free, capped by
`FANOUT_MAX_PARALLEL` (default 3). The binding limit is the provider's rate
limit, not the machine. A failed subtask cascades — everything downstream is
marked skipped rather than run against files that were never written.

`fanout plan <group>` shows the waves and each subtask's state; `fanout stop <group>`
stops the schedule and everything under it.

On the board a breakdown is one card rather than N. It carries the idea it was
split from, the subtask count and wave count, and a single badge for the whole
plan — running if any subtask is, then error, then stopped, and done only when
every subtask is. Expanding it lists the subtasks under their wave numbers;
**Files** shows the one workspace they share. It is open while the plan runs and
closed once it is not, until you say otherwise.

The card is the unit: dragging it files every subtask at once, and deleting it
removes all of them and the shared workspace behind one confirm. Moving a plan
anywhere but To-Do stops its schedule, the same way dragging a single running
task out of To-Do stops it.

## Seeding a workspace

`--seed` puts material in the workspace before anything runs, so an agent starts
from what you already have instead of an empty directory. It takes a file or a
directory and is repeatable:

```bash
fanout add "port it" --goal "port old.py to Go" --seed old.py --start
fanout breakdown "split this spec into pages" --seed spec.md --seed assets/ --start --watch
```

A file lands under its own name and a directory under its own name as a prefix —
`--seed assets/` arrives as `assets/...`, the same as copying the argument into
the workspace. Dotted names are skipped at every level, so `.git` and `.env` in a
working directory are never handed to an agent.

The CLI reads the paths and sends the contents with the request, because the
board may be on another machine. Text only: content travels as JSON and the
agent's tools are text tools, so a binary file is refused rather than corrupted.
Limits are 256 KB per file and 2 MB in total, enforced on both sides.

For a breakdown the seed is also shown to the planner, so the split is drawn
around files that exist rather than around files the subtasks have to invent.
Seeded files are unowned: any subtask may read one, and a subtask may claim one
in `writes` if its job is to replace it. A seed still reaches the single task the
idea falls back to when it cannot be split.

## Agent Tools

The agent gets a sandboxed workspace at `output/<task-id>/` and these tools:

| Tool | Arguments | Effect |
|------|-----------|--------|
| `write_file` | `path`, `content` | Create or overwrite a file |
| `read_file` | `path` | Read a file (truncated at 20 KB) |
| `edit_file` | `path`, `old`, `new` | Replace the first occurrence of `old` |
| `delete_file` | `path` | Delete a file |
| `list_files` | — | List the workspace |
| `run_command` | `command` | Run a shell command in the workspace (only with `FANOUT_SHELL=1`) |

Paths are relative and resolved inside the workspace; `..` escapes are rejected. An
absolute path is folded back onto the root, since the model is shown its workspace
directory in full and routinely answers with one — including, sometimes, a near miss at
the id, which is read as the path inside the workspace rather than recreated as a
directory tree under it.

The files the agent produces are listed in the task detail panel and live on disk under
`output/`. Within a breakdown that listing is the shared workspace, so each file is
marked with whether this task wrote it; `fanout files` shows a subtask's own output and
`--all` shows everything beside it.

Repeating yourself ends a run, but only when nothing moved: a call that does not change
the workspace is counted against the state it ran on, so reading a file back after
editing it, or re-running the tests that caught a bug, is progress rather than a loop.
Writing the same bytes to the same path is a repeat regardless.

## Shell commands

Off by default. `FANOUT_SHELL=1` gives agents `run_command`, so they can compile and test
what they write instead of only writing it.

The command line is never parsed, escaped, or matched against an allowlist. It goes
straight to `/bin/sh` inside a [bubblewrap](https://github.com/containers/bubblewrap)
jail, because the jail is the security boundary and there is nothing inside it to escape
to. That is also what keeps fanoutd language-agnostic: whatever toolchain the host has
under `/usr` is what an agent can run, and there is no per-language code in the server.

**bubblewrap is mandatory.** At startup the sandbox is probed by running a real command
through the real jail — not by looking for the binary, since an unprivileged user
namespace can be missing or blocked. If the probe fails the server logs why, `run_command`
is never advertised to the model, and a model that asks for it anyway is refused. There is
no unsandboxed fallback.

The workspace is mounted at **the same absolute path it has on the host**, and every
command starts there. That is not cosmetic: the model is told that path in its prompt and
the file tools accept it, so a shell that disagreed would make the prompt true for five
tools and false for the sixth. Mounting it somewhere tidier cost a run — `cd <workspace>`
failed, the agent concluded the directory did not exist, and `mkdir -p <workspace> && cd
<workspace> && go mod init` then reported complete success against a directory inside the
jail's own root that evaporated when the command exited.

Inside the jail:

- **No network.** A command cannot fetch dependencies, and cannot exfiltrate a workspace.
- **Nothing writable but the workspace.** `/usr` and `/etc` are read-only; the rest of the
  host is not mounted at all.
- **No environment.** `--clearenv`, so `OPENROUTER_API_KEY` is not in scope. Without this
  a command reads the key, it lands in the trace, and from there in the next prompt.
- **Resource limits** from a transient systemd user scope — bubblewrap has none of its own,
  and `--unshare-pid` contains a fork bomb's cleanup but not its appetite. Where there is
  no user manager the limits are skipped with a warning; the boundary is unaffected.
- **A wall-clock timeout**, which starts *after* a command acquires a slot, so queueing
  never eats its budget.
- **Capped output**, head and tail. Tool results are replayed into the next prompt, so an
  uncapped `find /` costs context and tokens rather than just scrollback.

**Build artifacts stay out of the workspace.** Each task gets a private `/build`
(`CARGO_TARGET_DIR`, `GOPATH`, `HOME`) and all tasks share `/cache` (`GOCACHE`,
`GOMODCACHE`, `CARGO_HOME`, npm). Private build directories matter for more than tidiness:
cargo takes an exclusive lock on its target directory, so a shared one would serialize
parallel subtasks into what looks like a hang. The shared caches are content-addressed or
self-locking, so concurrent use is safe and a second build does not start cold.

**Shell writes are reconciled against the claim table.** A command bypasses the
`write_file` path entirely, so without this the one-writer rule would hold only for the
tools that happen to go through it. The workspace is stamped before and after; paths the
task created become its own, exactly as an unplanned `write_file` would, and paths another
task owns are reported back in the same shape as a refused write. Already-written bytes
cannot be taken back, so this detects rather than prevents — which is also why serializing
builds would not have helped: sequential clobbering is still clobbering.

**Toolchains outside `/usr`** — rustup, nvm, pyenv, `go install` — need `FANOUT_SHELL_ROBIND`,
a colon-separated list of host paths to mount read-only. Any entry with a `bin/`
subdirectory joins `PATH`. It is empty by default because binding a home directory would
hand every agent your ssh keys.

`FANOUT_MAX_EXEC` caps concurrent commands and is unlimited by default. The cgroup limits
already bound the machine, and a global build lock would make the rolling-parallel
scheduler run one wide at exactly its most expensive step. Set it only if you see thrash.

See [.env.example](.env.example) for every knob.

## Setup

### 1. Install dependencies

```bash
# Backend
go mod download

# Frontend
bun install --cwd frontend
```

### 2. Configure

Set your OpenRouter API key:

```bash
export OPENROUTER_API_KEY=sk-or-v1-...
export OPENROUTER_MODEL=inclusionai/ling-3.0-flash:free  # optional, default
export PORT=8080  # optional, default
```

### 3. Run (development)

```bash
# Terminal 1: Frontend dev server
bun run --cwd frontend dev

# Terminal 2: Backend server
go run ./cmd/fanoutd
```

Or use the convenience script (builds both binaries, then starts frontend +
backend):

```bash
./run.sh
```

### 4. Production build

The frontend is embedded, so build it *before* the server — `go build` bakes in
whatever is in `cmd/fanoutd/dist/` at that moment.

```bash
bun run --cwd frontend build
go build -o fanoutd ./cmd/fanoutd
go build -o fanout ./cmd/fanout
```

The server is a single self-contained file. Copy it anywhere and run it; no
`dist/` directory required. `fanout` is a separate ~6 MB binary that belongs on
whatever machine you type from, not next to the server.

## Configuration

| Env Variable | Default | Description |
|-------------|---------|-------------|
| `OPENROUTER_API_KEY` | *(required)* | OpenRouter API key |
| `OPENROUTER_MODEL` | `inclusionai/ling-3.0-flash:free` | LLM model |
| `PORT` | `8080` | API server port |
| `FANOUT_TOKEN` | *(empty)* | Gates the API. Empty leaves the server open |
| `FANOUT_DATA_DIR` | `$XDG_DATA_HOME/fanoutd` | Database and workspaces |
| `DATABASE_PATH` | `fanoutd.db` | Relative to the data directory |
| `OUTPUT_DIR` | `output` | Relative to the data directory |
| `FANOUT_ENV_FILE` | *(see below)* | Override the settings file location |
| `OPENROUTER_BASE_URL` | OpenRouter | Override the API base URL (testing) |

### Rate limits

The default model is a free one, and the free tier is capped per minute across
the whole key — which a breakdown running three subtasks at once reaches easily.
A 429 is waited out rather than failed: the client retries five times, sleeping
until the reset OpenRouter names in `X-RateLimit-Reset` (it sends no
`Retry-After`, and on the free tier the reset is in the error body as well as the
headers), plus a small random spread so subtasks refused together do not all wake
at the same instant and collide again. Only a limit that outlasts all five ends
the run, and it says so.

Lower `FANOUT_MAX_PARALLEL` if you see it often; a paid model has far higher
limits and is the other way out.

### Paths

Nothing resolves against the working directory. `FANOUT_DATA_DIR` defaults to
`$XDG_DATA_HOME/fanoutd` (`~/.local/share/fanoutd`), and a relative
`DATABASE_PATH` or `OUTPUT_DIR` is taken relative to it. Absolute values are used
as given. The server behaves identically whether it is started from the repo,
from a systemd unit, or from `/`.

The frontend is compiled into the binary with `go:embed`, so deploying is one
file with no `dist/` directory beside it.

Settings are read from the first file that exists: `FANOUT_ENV_FILE`, then
`./.env`, then `$XDG_CONFIG_HOME/fanoutd/env`. Exported environment variables
win over the file either way.

## Authentication

Setting `FANOUT_TOKEN` gates every `/api` route except `/api/health` and the
auth endpoints. One secret, two transports:

- **`Authorization: Bearer <token>`** — for scripts and the CLI.
- **Session cookie** — the UI shows a login prompt, posts the token to
  `/api/auth/login`, and gets a 30-day `HttpOnly` cookie. This is what makes the
  board usable from a phone, where bearer headers are not practical.

Leaving the token empty keeps local development frictionless, but this server
runs an LLM agent that writes files and spends OpenRouter credits — set a token
before exposing it beyond localhost.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/auth` | Whether a token is required, and whether you have one |
| POST | `/api/auth/login` | Exchange a token for a session cookie |
| POST | `/api/auth/logout` | Clear the session cookie |

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/tasks` | List all tasks |
| POST | `/api/tasks` | Create task (starts in Ideas); optional `seed: [{path, content}]` |
| GET | `/api/tasks/:id` | Get task + trace |
| PUT | `/api/tasks/:id` | Update task |
| DELETE | `/api/tasks/:id` | Delete task |
| POST | `/api/tasks/:id/move` | Move column (stops the agent if moved out of To-Do) |
| POST | `/api/tasks/:id/start` | Start (or resume) the agent loop; 409 if already running |
| POST | `/api/tasks/:id/stop` | Cancel the running agent loop |
| GET | `/api/tasks/:id/trace` | Full trace breakdown |
| GET | `/api/tasks/:id/status` | Task status, error, and whether a run is live |
| GET | `/api/tasks/:id/files` | Files the agent has written |
| GET | `/api/tasks/:id/raw?path=` | One workspace file, served inline |
| POST | `/api/tasks/:id/continue` | New goal against the same workspace |
| POST | `/api/tasks/:id/retry` | Same brief, clean workspace |
| POST | `/api/breakdown` | Split an idea into a group of subtasks; blocks on the model; optional `seed: [{path, content}]` |
| GET | `/api/groups/:id/plan` | The resolved waves and every subtask's state |
| POST | `/api/groups/:id/start` | Run the schedule; 409 if already running |
| POST | `/api/groups/:id/stop` | Cancel the schedule and every subtask under it |
| GET | `/api/models` | Models this server accepts, and its default |
| GET | `/api/health` | Health check |

## Environment File

Copy `.env.example` to `.env` and add your OpenRouter API key:

```bash
cp .env.example .env
# Edit .env and add your key
```

For a deployed server, install the same file at
`$XDG_CONFIG_HOME/fanoutd/env` instead and leave the repo out of it.

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
| `show <id> [--last N] [--json]` | task, files, and its last few steps |
| `watch <id> [--interval 2s]` | prints steps as they land, exits when the run ends |
| `trace <id> [--last N] [--full] [--json]` | truncated by default |
| `start <id> [--watch]` / `stop <id>` | the agent loop |
| `mv <id> <column>` / `rm <id> [--keep-files]` | organize and delete |
| `files <id> [--all] [--abs]` / `cat <id> <path>` | workspace output |
| `continue <id> --goal ...` | new goal, same workspace |
| `retry <id> [--model ...]` | same brief, clean workspace |
| `models` | what this server accepts for `--model` |

**Output shaping.** `GET /api/tasks/:id/trace` returns every prompt and response
verbatim, which is unusable in a terminal and worse in an agent's context
window. Everything is compact by default; `--json` opts into machine-readable
output and `--full` opts into the raw dump.

**Id prefixes.** Any unambiguous prefix works, and so does part of a title —
`fanout show c762`, `fanout show Tetris`. Ambiguity is an error listing the candidates,
never a guess. Group ids resolve the same way, and a group is also named by any
of its subtasks' titles; `fanout show` prints the group a subtask belongs to.

**Exit codes.** `0` succeeded, `1` the command failed, `2` the command worked
and the *task* ended in `error`. That third one is what makes `fanout watch`
composable:

```bash
fanout start c762 --watch && ./deploy.sh
```

### Configuration

Precedence: `--server` → `FANOUT_URL` → config file → `http://localhost:8080`.
Same resolution for `--token` / `FANOUT_TOKEN`. The config file lives at
`$XDG_CONFIG_HOME/fanoutd/config.toml`, falling back to
`~/.config/fanoutd/config.toml`:

```toml
url = "https://board.example"
token = "..."
```

Nothing is derived from the working directory or the binary's location. A laptop
sets `url` once; localhost is only the default for when you are sitting on the
box.

## Roadmap

See [ROADMAP.md](ROADMAP.md) — repo seeding is what remains.
