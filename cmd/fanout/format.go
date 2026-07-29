package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"fanoutd/internal/models"
)

// The point of this file is that GET /api/tasks/:id/trace returns every prompt
// and response verbatim. That is unusable in a terminal and worse in an agent's
// context window, so nothing here prints a field at full length by default.

const (
	titleWidth  = 28
	actionWidth = 48
	detailWidth = 40
)

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// table prints aligned columns without padding the last one, so trailing detail
// can run long without dragging whitespace behind it.
func table(w io.Writer, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			if n := utf8.RuneCountInString(cell); i < len(widths) && n > widths[i] {
				widths[i] = n
			}
		}
	}
	for _, row := range rows {
		var b strings.Builder
		for i, cell := range row {
			if i == len(row)-1 {
				b.WriteString(cell)
				break
			}
			// Padded by rune count; %-*s counts bytes, which over-pads any
			// cell that a clip truncated.
			b.WriteString(cell)
			b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(cell)+2))
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}

// clip counts runes, not bytes: a model writes UTF-8 into actions and titles,
// and slicing one mid-rune puts mojibake in the table.
func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

// pad right-pads to a rune width, for output that streams a line at a time and
// so cannot be laid out by table(). Counting runes matters for the same reason
// clip does: %-*s counts bytes and over-pads anything non-ASCII.
func pad(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

// firstLine collapses a tool result to something that fits one row. Read and
// list results are many lines; write and edit results are already one.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// taskRow is one line of ls: the task plus the two numbers that say what is
// actually happening to it, which the task row alone does not carry.
type taskRow struct {
	Task  models.Task       `json:"task"`
	Steps int               `json:"steps"`
	Files int               `json:"files"`
	Last  *models.TraceStep `json:"last_step,omitempty"`
}

// progress is the "step 7" / "12 steps" column.
func (r taskRow) progress(running bool) string {
	if r.Steps == 0 {
		return "-"
	}
	if running {
		return fmt.Sprintf("step %d", r.Steps)
	}
	return plural(r.Steps, "step")
}

// detail is what the agent is doing right now, or what it left behind.
func (r taskRow) detail(running bool) string {
	if running && r.Last != nil {
		if r.Last.ToolName != "" {
			return clip(r.Last.ToolName+" "+firstLine(r.Last.ToolResult), detailWidth)
		}
		return clip(firstLine(r.Last.Action), detailWidth)
	}
	if r.Files > 0 {
		return plural(r.Files, "file")
	}
	if r.Last != nil && r.Last.ToolResult != "" && r.Task.Status == models.StatusError {
		return clip(firstLine(r.Last.ToolResult), detailWidth)
	}
	return ""
}

func printTaskRows(w io.Writer, rows []taskRow, running map[string]bool) {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		live := running[r.Task.ID] || r.Task.Status == models.StatusRunning
		out = append(out, []string{
			shortID(r.Task.ID),
			clip(r.Task.Title, titleWidth),
			r.Task.Column,
			r.Task.Status,
			r.progress(live),
			r.detail(live),
		})
	}
	table(w, out)
}

// printTrace is the compact trace: one line per step. --full is what dumps the
// verbatim prompt and response.
func printTrace(w io.Writer, steps []models.TraceStep) {
	rows := [][]string{{"STEP", "ACTION", "TOOL", "RESULT"}}
	for _, s := range steps {
		rows = append(rows, []string{
			fmt.Sprintf("%d", s.StepNumber),
			clip(firstLine(s.Action), actionWidth),
			s.ToolName,
			clip(firstLine(s.ToolResult), detailWidth),
		})
	}
	table(w, rows)
}

func printFullTrace(w io.Writer, steps []models.TraceStep) {
	for i, s := range steps {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "── step %d  %s\n", s.StepNumber, s.Timestamp.Local().Format(time.RFC3339))
		if s.Action != "" {
			fmt.Fprintf(w, "action: %s\n", s.Action)
		}
		if s.Prompt != "" {
			fmt.Fprintf(w, "\nprompt:\n%s\n", s.Prompt)
		}
		if s.Response != "" {
			fmt.Fprintf(w, "\nresponse:\n%s\n", s.Response)
		}
		if s.ToolName != "" {
			fmt.Fprintf(w, "\ntool: %s\n", s.ToolName)
		}
		if s.ToolResult != "" {
			fmt.Fprintf(w, "\nresult:\n%s\n", s.ToolResult)
		}
	}
}

// stepLine is the one-line form watch prints as each step lands.
func stepLine(s models.TraceStep) string {
	parts := []string{fmt.Sprintf("%3d", s.StepNumber)}
	if a := clip(firstLine(s.Action), actionWidth); a != "" {
		parts = append(parts, a)
	}
	if s.ToolName != "" {
		parts = append(parts, "→ "+s.ToolName+" "+clip(firstLine(s.ToolResult), detailWidth))
	} else if s.ToolResult != "" {
		parts = append(parts, "→ "+clip(firstLine(s.ToolResult), detailWidth))
	}
	return strings.Join(parts, "  ")
}
