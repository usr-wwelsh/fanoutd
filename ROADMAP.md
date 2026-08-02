# Roadmap

Steps 1 (portability + auth), 2 (the `fanout` CLI) and 4 (idea breakdown) are
done; what they shipped is in the README and what they got wrong is in the git
history. Step 3, repo seeding, is what remains.

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
thousand-entry enumeration would blow the intro budget on its own, which the
transcript budget in `replayTrace` does not cover because the intro is not part
of the trace.

---

## Tests

Not a phase — folded into each step as it lands. Coverage is broad: the response
parser, the CLI end to end, streaming and tool-call fragment reassembly, the
concede path, shutdown and reclaim, `Workspace.resolve`, claims and scheduling
under `-race`, the breakdown parser and whole breakdown runs, the group routes,
the transcript budget, and `nextTitle`.

Nothing on the existing surface is knowingly untested. Step 3 brings its own:
the clone caps, the refusal to seed once a trace exists, and whatever shape the
seeded-workspace intro settles on.
