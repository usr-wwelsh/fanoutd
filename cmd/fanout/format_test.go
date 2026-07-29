package main

import (
	"bytes"
	"strings"
	"testing"

	"fanoutd/internal/models"
)

func TestClip(t *testing.T) {
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{"short", 10, "short"},
		{"exactly-ten", 11, "exactly-ten"},
		{"a much longer title than fits", 10, "a much lo…"},
		{"  padded  ", 10, "padded"},
	}
	for _, tt := range tests {
		if got := clip(tt.in, tt.max); got != tt.want {
			t.Errorf("clip(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("wrote a.md (12 bytes)"); got != "wrote a.md (12 bytes)" {
		t.Errorf("got %q", got)
	}
	// A read_file result is the whole file; only the first line can go in a row.
	if got := firstLine("line one\nline two\nline three"); got != "line one" {
		t.Errorf("got %q", got)
	}
}

func TestHumanSize(t *testing.T) {
	tests := map[int64]string{0: "0 B", 512: "512 B", 2048: "2.0 KB", 5 * 1024 * 1024: "5.0 MB"}
	for in, want := range tests {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestPlural(t *testing.T) {
	if got := plural(1, "step"); got != "1 step" {
		t.Errorf("got %q", got)
	}
	if got := plural(12, "step"); got != "12 steps" {
		t.Errorf("got %q", got)
	}
}

func TestTableAlignsAllButTheLastColumn(t *testing.T) {
	var buf bytes.Buffer
	table(&buf, [][]string{
		{"a", "long-value", "x"},
		{"bbbb", "v", "y"},
	})
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines", len(lines))
	}
	if !strings.HasPrefix(lines[0], "a     long-value  x") {
		t.Errorf("line 0 = %q", lines[0])
	}
	// No trailing whitespace: the last column is not padded.
	for _, l := range lines {
		if l != strings.TrimRight(l, " ") {
			t.Errorf("line %q has trailing space", l)
		}
	}
}

func TestTaskRowProgressAndDetail(t *testing.T) {
	running := taskRow{
		Task:  models.Task{ID: "c762903a", Title: "Tetris clone", Status: models.StatusRunning},
		Steps: 7,
		Last: &models.TraceStep{
			StepNumber: 7,
			Action:     "writing the board renderer",
			ToolName:   "write_file",
			ToolResult: "wrote tetris.html (4210 bytes)\nnext line",
		},
	}
	if got := running.progress(true); got != "step 7" {
		t.Errorf("progress = %q", got)
	}
	// Tool plus the first line of its result, clipped to the column.
	if got := running.detail(true); got != "write_file wrote tetris.html (4210 byte…" {
		t.Errorf("detail = %q", got)
	}
	if len(running.detail(true)) > detailWidth+2 {
		t.Errorf("detail is wider than its column: %q", running.detail(true))
	}

	finished := taskRow{
		Task:  models.Task{Status: models.StatusDone},
		Steps: 12,
		Files: 3,
		Last:  &models.TraceStep{Action: "goal met"},
	}
	if got := finished.progress(false); got != "12 steps" {
		t.Errorf("progress = %q", got)
	}
	if got := finished.detail(false); got != "3 files" {
		t.Errorf("detail = %q", got)
	}

	fresh := taskRow{Task: models.Task{Status: models.StatusIdle}}
	if got := fresh.progress(false); got != "-" {
		t.Errorf("progress = %q", got)
	}
	if got := fresh.detail(false); got != "" {
		t.Errorf("detail = %q", got)
	}
}

// A running step where the model only thought — no tool call — still says
// something useful.
func TestTaskRowDetailWithoutTool(t *testing.T) {
	r := taskRow{
		Task:  models.Task{Status: models.StatusRunning},
		Steps: 2,
		Last:  &models.TraceStep{Action: "planning the file layout"},
	}
	if got := r.detail(true); got != "planning the file layout" {
		t.Errorf("detail = %q", got)
	}
}

func TestPrintTraceIsOneLinePerStep(t *testing.T) {
	// The whole point: a trace step carries a verbatim response, and the
	// compact view must not print it.
	steps := []models.TraceStep{
		{StepNumber: 1, Action: "writing the page", Response: strings.Repeat("x", 5000), ToolName: "write_file", ToolResult: "wrote index.html (12 bytes)"},
		{StepNumber: 2, Action: "goal met", Response: strings.Repeat("y", 5000)},
	}
	var buf bytes.Buffer
	printTrace(&buf, steps)
	out := buf.String()

	if strings.Contains(out, "xxxxx") || strings.Contains(out, "yyyyy") {
		t.Error("compact trace leaked a verbatim response")
	}
	if n := strings.Count(strings.TrimRight(out, "\n"), "\n") + 1; n != 3 {
		t.Errorf("got %d lines, want a header plus two steps", n)
	}
	if !strings.Contains(out, "wrote index.html") {
		t.Errorf("missing the tool result:\n%s", out)
	}
}

func TestPrintFullTraceKeepsEverything(t *testing.T) {
	steps := []models.TraceStep{{StepNumber: 1, Action: "a", Response: "VERBATIM", ToolName: "write_file", ToolResult: "wrote a.md"}}
	var buf bytes.Buffer
	printFullTrace(&buf, steps)
	for _, want := range []string{"VERBATIM", "write_file", "wrote a.md"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("--full output is missing %q", want)
		}
	}
}

func TestLastSteps(t *testing.T) {
	steps := []models.TraceStep{{StepNumber: 1}, {StepNumber: 2}, {StepNumber: 3}}
	if got := lastSteps(steps, 0); len(got) != 3 {
		t.Errorf("0 means all, got %d", len(got))
	}
	if got := lastSteps(steps, 10); len(got) != 3 {
		t.Errorf("more than exist means all, got %d", len(got))
	}
	got := lastSteps(steps, 2)
	if len(got) != 2 || got[0].StepNumber != 2 {
		t.Errorf("got %+v", got)
	}
}

func TestFilterTasks(t *testing.T) {
	tasks := []models.Task{
		{ID: "1", Column: "todo", Status: models.StatusRunning},
		{ID: "2", Column: "todo", Status: models.StatusError},
		{ID: "3", Column: "finished", Status: models.StatusDone},
	}
	if got := filterTasks(tasks, "", ""); len(got) != 3 {
		t.Errorf("no filter should pass everything, got %d", len(got))
	}
	if got := filterTasks(tasks, "todo", ""); len(got) != 2 {
		t.Errorf("got %d", len(got))
	}
	if got := filterTasks(tasks, "TODO", "RUNNING"); len(got) != 1 || got[0].ID != "1" {
		t.Errorf("filters should be case-insensitive, got %+v", got)
	}
	if got := filterTasks(tasks, "ideas", ""); len(got) != 0 {
		t.Errorf("got %d", len(got))
	}
}

func TestMatchFile(t *testing.T) {
	files := []models.FileEntry{
		{Path: "index.html"},
		{Path: "src/board.js"},
		{Path: "docs/board.js"},
	}

	t.Run("exact", func(t *testing.T) {
		got, err := matchFile(files, "src/board.js")
		if err != nil || got != "src/board.js" {
			t.Fatalf("got (%q, %v)", got, err)
		}
	})

	t.Run("leading ./ is tolerated", func(t *testing.T) {
		got, err := matchFile(files, "./index.html")
		if err != nil || got != "index.html" {
			t.Fatalf("got (%q, %v)", got, err)
		}
	})

	t.Run("unique basename", func(t *testing.T) {
		got, err := matchFile(files, "index.html")
		if err != nil || got != "index.html" {
			t.Fatalf("got (%q, %v)", got, err)
		}
	})

	t.Run("ambiguous basename lists candidates", func(t *testing.T) {
		_, err := matchFile(files, "board.js")
		if err == nil {
			t.Fatal("expected an ambiguity error")
		}
		if !strings.Contains(err.Error(), "src/board.js") || !strings.Contains(err.Error(), "docs/board.js") {
			t.Errorf("error %q should name both", err)
		}
	})

	t.Run("missing lists what exists", func(t *testing.T) {
		_, err := matchFile(files, "nope.txt")
		if err == nil || !strings.Contains(err.Error(), "index.html") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("empty workspace", func(t *testing.T) {
		if _, err := matchFile(nil, "a.txt"); err == nil {
			t.Fatal("expected an error")
		}
	})
}
