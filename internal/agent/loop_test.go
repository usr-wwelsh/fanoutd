package agent

import (
	"strings"
	"testing"

	"fanoutd/internal/llm"
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
	resp := &llm.Result{
		Content: "Writing the board renderer.",
		ToolCalls: []llm.ToolCall{{
			Function: llm.FunctionCall{
				Name:      "write_file",
				Arguments: `{"path":"tetris.html","content":"<html>"}`,
			},
		}},
	}
	got, err := parseResponse(resp)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Call.Name != "write_file" || got.Tools[0].Call.Path != "tetris.html" {
		t.Fatalf("tools = %+v", got.Tools)
	}
	if got.Action != "Writing the board renderer." || got.Synthesized {
		t.Errorf("action = %q synthesized=%v, want the model's own text", got.Action, got.Synthesized)
	}
}

func TestParseResponseSynthesizesActionWhenSilent(t *testing.T) {
	resp := &llm.Result{
		ToolCalls: []llm.ToolCall{{
			Function: llm.FunctionCall{Name: "READ_FILE", Arguments: `{"path":"spec.md"}`},
		}},
	}
	got, err := parseResponse(resp)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	// Name is lowercased so the repeat counter and Exec see one spelling.
	if len(got.Tools) != 1 || got.Tools[0].Call.Name != "read_file" {
		t.Errorf("tools = %+v, want one read_file", got.Tools)
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
	resp := &llm.Result{
		ToolCalls: []llm.ToolCall{{
			Function: llm.FunctionCall{Name: "finish", Arguments: `{"summary":"wrote tetris.html"}`},
		}},
	}
	got, err := parseResponse(resp)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if !got.GoalMet || got.Summary != "wrote tetris.html" {
		t.Errorf("got %+v, want goal met with a summary", got)
	}
	if len(got.Tools) != 0 {
		t.Error("finish is not a workspace operation and must not be executed")
	}
}

// A model that writes its last file and signs off in the same turn must get
// both: returning on the finish alone discards the write it was signing off on.
func TestParseResponseFinishAlongsideWork(t *testing.T) {
	resp := &llm.Result{
		ToolCalls: []llm.ToolCall{
			{ID: "c1", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"a.md","content":"hi"}`}},
			{ID: "c2", Function: llm.FunctionCall{Name: "finish", Arguments: `{"summary":"wrote a.md"}`}},
		},
	}
	got, err := parseResponse(resp)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if !got.GoalMet || got.Summary != "wrote a.md" {
		t.Errorf("got goal_met=%v summary=%q, want the finish to be honoured", got.GoalMet, got.Summary)
	}
	if len(got.Tools) != 1 || got.Tools[0].Call.Path != "a.md" {
		t.Fatalf("tools = %+v, want the write still pending", got.Tools)
	}
}

// Every call in a turn has to survive parsing, with the ids their results will
// be keyed on.
func TestParseResponseKeepsEveryToolCall(t *testing.T) {
	resp := &llm.Result{
		ToolCalls: []llm.ToolCall{
			{ID: "c1", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"a.md","content":"a"}`}},
			{ID: "c2", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"b.md","content":"b"}`}},
			{ID: "c3", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"c.md","content":"c"}`}},
		},
	}
	got, err := parseResponse(resp)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if len(got.Tools) != 3 {
		t.Fatalf("kept %d of 3 calls", len(got.Tools))
	}
	for i, want := range []string{"c1", "c2", "c3"} {
		if got.Tools[i].ID != want {
			t.Errorf("call %d has id %q, want %q — in the order the model made them", i, got.Tools[i].ID, want)
		}
		if got.Tools[i].Args == "" {
			t.Errorf("call %d kept no arguments to replay", i)
		}
	}
}

func TestParseResponseJSONFallback(t *testing.T) {
	resp := &llm.Result{
		Content: "```json\n{\"goal_met\":false,\"next_action\":\"write the page\",\"tool\":{\"name\":\"write_file\",\"path\":\"index.html\",\"content\":\"hi\"}}\n```",
	}
	got, err := parseResponse(resp)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if got.Action != "write the page" || len(got.Tools) != 1 || got.Tools[0].Call.Path != "index.html" {
		t.Fatalf("got %+v", got)
	}
	// The fallback protocol carries no id, so this step replays as prose.
	if got.Tools[0].ID != "" {
		t.Errorf("id = %q, want empty", got.Tools[0].ID)
	}
	if got.Synthesized {
		t.Error("the model described the action itself")
	}
}

func TestParseResponseErrors(t *testing.T) {
	tests := []struct {
		name string
		resp *llm.Result
	}{
		{"prose only", &llm.Result{Content: "I'll write the file now."}},
		{"tool call with no name", &llm.Result{
			ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Arguments: "{}"}}},
		}},
		{"tool call with bad arguments", &llm.Result{
			ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Name: "write_file", Arguments: "path=a.txt"}}},
		}},
		{"xml tool call", &llm.Result{Content: "<tool_call>write_file</tool_call>"}},
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
	resp := &llm.Result{
		ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Name: "list_files"}}},
	}
	got, err := parseResponse(resp)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Call.Name != "list_files" {
		t.Fatalf("tools = %+v", got.Tools)
	}
}
