package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"fanoutd/internal/models"
)

func cmdFiles(e *env, args []string) error {
	fs := e.flags("files")
	asJSON := fs.Bool("json", false, "machine-readable output")
	abs := fs.Bool("abs", false, "print on-disk paths instead of workspace-relative ones")
	all := fs.Bool("all", false, "include files written by sibling subtasks sharing the workspace")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fanout files <id> [--all] [--abs] [--json]")
		fs.PrintDefaults()
	}
	id, err := e.resolve(fs, args)
	if err != nil {
		return e.describeErr(err)
	}
	files, err := e.client.Files(e.ctx, id)
	if err != nil {
		return e.describeErr(err)
	}

	// The task's own files by default. A subtask of a breakdown shares its
	// workspace, so the whole listing is its siblings' work as much as its own
	// and answers "what did this produce?" with everyone's answer at once.
	shown := files
	if !*all {
		shown = ownedFiles(files)
	}

	if *asJSON {
		return writeJSON(e.out, shown)
	}
	if len(shown) == 0 {
		fmt.Fprintln(e.out, "no files")
		if len(files) > 0 {
			fmt.Fprintf(e.out, "%s in the shared workspace belong to sibling subtasks; --all lists them\n", plural(len(files), "file"))
		}
		return nil
	}
	rows := make([][]string, 0, len(shown))
	for _, f := range shown {
		name := f.Path
		if *abs {
			name = f.Abs
		}
		if *all && !f.Owned {
			name += "  (sibling)"
		}
		rows = append(rows, []string{humanSize(f.Size), f.Modified.Local().Format(time.RFC3339), name})
	}
	table(e.out, rows)
	if hidden := len(files) - len(shown); hidden > 0 {
		fmt.Fprintf(e.out, "\n%s from sibling subtasks not shown (--all)\n", plural(hidden, "file"))
	}
	return nil
}

// ownedFiles keeps the entries a task actually wrote. A task alone in its
// workspace owns all of them, so this only narrows anything for a subtask of a
// breakdown — and an empty result there is the true answer, not a degraded one:
// it is a subtask that produced nothing while its siblings produced plenty.
func ownedFiles(files []models.FileEntry) []models.FileEntry {
	out := make([]models.FileEntry, 0, len(files))
	for _, f := range files {
		if f.Owned {
			out = append(out, f)
		}
	}
	return out
}

func cmdCat(e *env, args []string) error {
	fs := e.flags("cat")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fanout cat <id> <path>")
		fs.PrintDefaults()
	}
	id, err := e.resolve(fs, args)
	if err != nil {
		return e.describeErr(err)
	}
	if fs.NArg() < 2 {
		fs.Usage()
		return fmt.Errorf("a file path is required")
	}

	files, err := e.client.Files(e.ctx, id)
	if err != nil {
		return e.describeErr(err)
	}
	path, err := matchFile(files, fs.Arg(1))
	if err != nil {
		return err
	}

	data, err := e.client.Raw(e.ctx, id, path)
	if err != nil {
		return e.describeErr(err)
	}
	_, err = e.out.Write(data)
	return err
}

// matchFile resolves a file argument the same way ids are resolved: exact
// first, then an unambiguous basename or suffix. Agent output nests, and
// retyping "src/components/board.svelte" to read it is the same friction as
// retyping a UUID.
func matchFile(files []models.FileEntry, want string) (string, error) {
	want = strings.TrimPrefix(strings.TrimSpace(want), "./")
	if want == "" {
		return "", fmt.Errorf("a file path is required")
	}

	var partial []string
	for _, f := range files {
		if f.Path == want {
			return f.Path, nil
		}
		if filepath.Base(f.Path) == want || strings.HasSuffix(f.Path, "/"+want) {
			partial = append(partial, f.Path)
		}
	}

	switch len(partial) {
	case 1:
		return partial[0], nil
	case 0:
		if len(files) == 0 {
			return "", fmt.Errorf("this task has no files")
		}
		var b strings.Builder
		fmt.Fprintf(&b, "no file named %q. this task has:", want)
		for _, f := range files {
			fmt.Fprintf(&b, "\n  %s", f.Path)
		}
		return "", fmt.Errorf("%s", b.String())
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%q matches %d files:", want, len(partial))
		for _, p := range partial {
			fmt.Fprintf(&b, "\n  %s", p)
		}
		return "", fmt.Errorf("%s", b.String())
	}
}

func cmdTrace(e *env, args []string) error {
	fs := e.flags("trace")
	last := fs.Int("last", 0, "only the last N steps (0 for all)")
	full := fs.Bool("full", false, "the raw dump: every prompt and response verbatim")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fanout trace <id> [--last N] [--full] [--json]")
		fs.PrintDefaults()
	}
	id, err := e.resolve(fs, args)
	if err != nil {
		return e.describeErr(err)
	}
	steps, err := e.client.Trace(e.ctx, id)
	if err != nil {
		return e.describeErr(err)
	}
	steps = lastSteps(steps, *last)

	switch {
	case *asJSON:
		return writeJSON(e.out, steps)
	case len(steps) == 0:
		fmt.Fprintln(e.out, "no steps yet")
	case *full:
		printFullTrace(e.out, steps)
	default:
		printTrace(e.out, steps)
	}
	return nil
}
