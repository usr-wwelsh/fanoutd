package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"fanoutd/internal/models"
)

// The transcript is replayed in full on every step, so its size is what a long
// run spends its context on. These pin the budget that keeps it bounded.

// fatStep is a step that wrote a large file, which is the shape that makes a
// transcript grow: the content sits in the call arguments and again in nothing
// else, and the result is short.
func fatStep(n int, size int) models.TraceStep {
	body := strings.Repeat("x", size)
	args, _ := json.Marshal(map[string]string{"path": fmt.Sprintf("f%d.md", n), "content": body})
	return models.TraceStep{
		StepNumber: n,
		Response:   fmt.Sprintf("writing f%d.md", n),
		Calls: []models.ToolExchange{{
			ID:        fmt.Sprintf("c%d", n),
			Name:      "write_file",
			Arguments: string(args),
			Result:    fmt.Sprintf("wrote f%d.md (%d bytes)", n, size),
		}},
	}
}

func TestTranscriptStaysWithinBudget(t *testing.T) {
	trace := make([]models.TraceStep, 0, 40)
	for i := 1; i <= 40; i++ {
		trace = append(trace, fatStep(i, 6000))
	}

	unbounded := 0
	for _, ts := range trace {
		unbounded += blocksSize(replayStep(ts, true))
	}

	msgs := replayTrace(trace)
	got := blocksSize(msgs)
	if got > transcriptBudget*2 {
		t.Errorf("replayed %d bytes, want within twice the %d budget (unbounded would be %d)", got, transcriptBudget, unbounded)
	}
	if got >= unbounded {
		t.Errorf("replayed %d bytes, which is no better than the %d an unbounded replay costs", got, unbounded)
	}
}

// Condensing must not break the pairing: every tool message has to answer a call
// the assistant turn actually made, or providers reject the request outright.
func TestCondensedStepsKeepTheirCallPairing(t *testing.T) {
	trace := make([]models.TraceStep, 0, 30)
	for i := 1; i <= 30; i++ {
		trace = append(trace, fatStep(i, 6000))
	}

	made := map[string]bool{}
	for _, m := range replayTrace(trace) {
		switch m.Role {
		case "assistant":
			for _, c := range m.ToolCalls {
				made[c.ID] = true
				if !json.Valid([]byte(c.Function.Arguments)) {
					t.Fatalf("call %s replayed with invalid JSON arguments: %s", c.ID, c.Function.Arguments)
				}
			}
		case "tool":
			if !made[m.ToolCallID] {
				t.Fatalf("tool message answers %s, which no assistant turn called", m.ToolCallID)
			}
		}
	}
	if len(made) != len(trace) {
		t.Fatalf("replayed %d calls, want %d — a step was dropped rather than condensed", len(made), len(trace))
	}
}

// The steps the model is about to react to are the ones it cannot do without.
func TestNewestStepsReplayWhole(t *testing.T) {
	trace := make([]models.TraceStep, 0, 30)
	for i := 1; i <= 30; i++ {
		trace = append(trace, fatStep(i, 6000))
	}

	msgs := replayTrace(trace)
	for _, ts := range trace[len(trace)-minFullSteps:] {
		want := ts.Calls[0].Arguments
		found := false
		for _, m := range msgs {
			for _, c := range m.ToolCalls {
				if c.Function.Arguments == want {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("step %d was condensed, but the newest %d steps must replay whole", ts.StepNumber, minFullSteps)
		}
	}
}

// A short run must be unaffected: the budget is there for long ones.
func TestShortTranscriptIsNotCondensed(t *testing.T) {
	trace := []models.TraceStep{fatStep(1, 200), fatStep(2, 200)}

	msgs := replayTrace(trace)
	for _, ts := range trace {
		found := false
		for _, m := range msgs {
			for _, c := range m.ToolCalls {
				if c.Function.Arguments == ts.Calls[0].Arguments {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("step %d was condensed in a two-step trace", ts.StepNumber)
		}
	}
}

// condenseArgs drops what a file's content costs and keeps what identifies the
// call, so an old step still says which path it wrote.
func TestCondenseArgsKeepsTheShortFields(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"path": "board.js", "content": strings.Repeat("y", 5000)})

	got := condenseArgs(string(args))
	if len(got) > digestBytes*2 {
		t.Errorf("condensed to %d bytes, want near the %d digest", len(got), digestBytes)
	}

	var fields map[string]string
	if err := json.Unmarshal([]byte(got), &fields); err != nil {
		t.Fatalf("condensed arguments are not valid JSON: %v", err)
	}
	if fields["path"] != "board.js" {
		t.Errorf("path came back as %q, want board.js — the field that identifies the call must survive", fields["path"])
	}
	if !strings.Contains(fields["content"], "elided") {
		t.Errorf("content came back as %q, want a note that it was elided", fields["content"])
	}
}

// Arguments that are not an object at all still have to leave valid JSON behind.
func TestCondenseArgsFallsBackToANote(t *testing.T) {
	got := condenseArgs(strings.Repeat("not json", 200))
	if !json.Valid([]byte(got)) {
		t.Fatalf("condensed unparseable arguments into invalid JSON: %s", got)
	}
}
