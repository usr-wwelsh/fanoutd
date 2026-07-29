package agent

import (
	"strings"
	"testing"

	"fanoutd/internal/openrouter"
)

// These cover the hand-rolled extraction of JSON from model output. It fails in
// ways that look like model problems rather than bugs, which is exactly why it
// is worth pinning down.

func TestStripFences(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no fence", `{"goal_met": true}`, `{"goal_met": true}`},
		{"json fence", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"bare fence", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"prose before fence", "Sure!\n```json\n{\"a\":1}\n```", `{"a":1}`},
		{"unclosed fence", "```json\n{\"a\":1}", `{"a":1}`},
		{"fence with no newline after tag", "```{\"a\":1}```", `{"a":1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripFences(tt.in); got != tt.want {
				t.Errorf("stripFences(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMatchBrace(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		start   int
		wantEnd int
		wantOK  bool
	}{
		{"flat", `{"a":1}`, 0, 6, true},
		{"nested", `{"a":{"b":2}}`, 0, 12, true},
		{"brace in string", `{"a":"}"}`, 0, 8, true},
		{"escaped quote then brace", `{"a":"\"}"}`, 0, 10, true},
		{"escaped backslash ends string", `{"a":"c:\\"}`, 0, 11, true},
		{"unterminated", `{"a":1`, 0, 0, false},
		{"trailing content ignored", `{"a":1} and more`, 0, 6, true},
		{"inner object", `{"a":{"b":2}}`, 5, 11, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			end, ok := matchBrace(tt.in, tt.start)
			if ok != tt.wantOK || (ok && end != tt.wantEnd) {
				t.Errorf("matchBrace(%q, %d) = (%d, %v), want (%d, %v)",
					tt.in, tt.start, end, ok, tt.wantEnd, tt.wantOK)
			}
		})
	}
}

func TestExtractJSON(t *testing.T) {
	t.Run("plain object", func(t *testing.T) {
		got, ok := extractJSON(`{"goal_met":false,"next_action":"go"}`)
		if !ok || got != `{"goal_met":false,"next_action":"go"}` {
			t.Fatalf("got (%q, %v)", got, ok)
		}
	})

	// The reason extractJSON scans candidates instead of taking first-{
	// through last-}: file content routinely contains braces.
	t.Run("skips a JS object in file content", func(t *testing.T) {
		in := `{"goal_met":false,"next_action":"write","tool":{"name":"write_file","path":"a.js","content":"const x = {a:1};\nfunction f(){ return {b:2}; }"}}`
		got, ok := extractJSON(in)
		if !ok {
			t.Fatal("expected a match")
		}
		if got != in {
			t.Errorf("got %q, want the whole envelope", got)
		}
	})

	t.Run("prose wrapper", func(t *testing.T) {
		in := "Here is my next step:\n" + `{"next_action":"read the spec"}` + "\nLet me know."
		got, ok := extractJSON(in)
		if !ok || got != `{"next_action":"read the spec"}` {
			t.Fatalf("got (%q, %v)", got, ok)
		}
	})

	t.Run("ignores a leading object with no agent key", func(t *testing.T) {
		in := `{"role":"assistant"} {"goal_met":true,"summary":"done"}`
		got, ok := extractJSON(in)
		if !ok || got != `{"goal_met":true,"summary":"done"}` {
			t.Fatalf("got (%q, %v)", got, ok)
		}
	})

	t.Run("no object at all", func(t *testing.T) {
		if _, ok := extractJSON("I will now write the file."); ok {
			t.Error("expected no match")
		}
	})

	t.Run("object with no agent key", func(t *testing.T) {
		if _, ok := extractJSON(`{"foo":1,"bar":2}`); ok {
			t.Error("expected no match")
		}
	})

	t.Run("bounded scan on brace soup", func(t *testing.T) {
		// maxJSONCandidates cuts the scan off; a real envelope past the bound
		// is missed, which is the deliberate trade. Pin the bound so a change
		// to it is a decision rather than an accident.
		in := strings.Repeat("{", maxJSONCandidates+10) + `{"goal_met":true}`
		if _, ok := extractJSON(in); ok {
			t.Error("expected the candidate scan to give up")
		}
	})
}

func TestParseResponseNativeToolCall(t *testing.T) {
	resp := &openrouter.Result{
		Content: "Writing the board renderer.",
		ToolCalls: []openrouter.ToolCall{{
			Function: openrouter.FunctionCall{
				Name:      "write_file",
				Arguments: `{"path":"tetris.html","content":"<html>"}`,
			},
		}},
	}
	got, err := parseResponse(resp)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if got.Tool == nil || got.Tool.Name != "write_file" || got.Tool.Path != "tetris.html" {
		t.Fatalf("tool = %+v", got.Tool)
	}
	if got.Action != "Writing the board renderer." || got.Synthesized {
		t.Errorf("action = %q synthesized=%v, want the model's own text", got.Action, got.Synthesized)
	}
}

func TestParseResponseSynthesizesActionWhenSilent(t *testing.T) {
	resp := &openrouter.Result{
		ToolCalls: []openrouter.ToolCall{{
			Function: openrouter.FunctionCall{Name: "READ_FILE", Arguments: `{"path":"spec.md"}`},
		}},
	}
	got, err := parseResponse(resp)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	// Name is lowercased so the repeat counter and Exec see one spelling.
	if got.Tool.Name != "read_file" {
		t.Errorf("name = %q, want read_file", got.Tool.Name)
	}
	if !got.Synthesized {
		t.Error("want Synthesized, since the model wrote no action text")
	}
	if got.Text != "" {
		t.Errorf("Text = %q, want empty — a synthesized label must not be replayed as the assistant turn", got.Text)
	}
	if got.Action != "[called read_file on spec.md]" {
		t.Errorf("action = %q", got.Action)
	}
}

func TestParseResponseFinishTool(t *testing.T) {
	resp := &openrouter.Result{
		ToolCalls: []openrouter.ToolCall{{
			Function: openrouter.FunctionCall{Name: "finish", Arguments: `{"summary":"wrote tetris.html"}`},
		}},
	}
	got, err := parseResponse(resp)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if !got.GoalMet || got.Summary != "wrote tetris.html" {
		t.Errorf("got %+v, want goal met with a summary", got)
	}
	if got.Tool != nil {
		t.Error("finish is not a workspace operation and must not be executed")
	}
}

func TestParseResponseJSONFallback(t *testing.T) {
	resp := &openrouter.Result{
		Content: "```json\n{\"goal_met\":false,\"next_action\":\"write the page\",\"tool\":{\"name\":\"write_file\",\"path\":\"index.html\",\"content\":\"hi\"}}\n```",
	}
	got, err := parseResponse(resp)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if got.Action != "write the page" || got.Tool == nil || got.Tool.Path != "index.html" {
		t.Fatalf("got %+v", got)
	}
	if got.Synthesized {
		t.Error("the model described the action itself")
	}
}

func TestParseResponseErrors(t *testing.T) {
	tests := []struct {
		name string
		resp *openrouter.Result
	}{
		{"prose only", &openrouter.Result{Content: "I'll write the file now."}},
		{"tool call with no name", &openrouter.Result{
			ToolCalls: []openrouter.ToolCall{{Function: openrouter.FunctionCall{Arguments: "{}"}}},
		}},
		{"tool call with bad arguments", &openrouter.Result{
			ToolCalls: []openrouter.ToolCall{{Function: openrouter.FunctionCall{Name: "write_file", Arguments: "path=a.txt"}}},
		}},
		{"xml tool call", &openrouter.Result{Content: "<tool_call>write_file</tool_call>"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseResponse(tt.resp); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// A tool call with no arguments at all is legal for list_files, which takes none.
func TestParseResponseToolCallWithoutArguments(t *testing.T) {
	resp := &openrouter.Result{
		ToolCalls: []openrouter.ToolCall{{Function: openrouter.FunctionCall{Name: "list_files"}}},
	}
	got, err := parseResponse(resp)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if got.Tool == nil || got.Tool.Name != "list_files" {
		t.Fatalf("tool = %+v", got.Tool)
	}
}
