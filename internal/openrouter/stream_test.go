package openrouter

import (
	"bufio"
	"strings"
	"testing"
)

func consume(t *testing.T, body string) (*Result, error) {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELine)
	return consumeStream(scanner, func() {})
}

func TestConsumeStreamContent(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"Hello"}}]}

data: {"choices":[{"delta":{"content":", "}}]}

data: {"choices":[{"delta":{"content":"world"}}]}

data: [DONE]
`
	got, err := consume(t, body)
	if err != nil {
		t.Fatalf("consumeStream: %v", err)
	}
	if got.Content != "Hello, world" {
		t.Errorf("content = %q, want %q", got.Content, "Hello, world")
	}
	if len(got.ToolCalls) != 0 {
		t.Errorf("got %d tool calls, want 0", len(got.ToolCalls))
	}
}

// A tool call arrives as fragments: identity on the first, then the arguments
// string split across frames. Concatenating them in order is the whole job, and
// getting it wrong yields JSON that will not parse.
func TestConsumeStreamToolCallFragments(t *testing.T) {
	body := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write_file","arguments":""}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\","}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"content\":\"hi\"}"}}]}}]}

data: [DONE]
`
	got, err := consume(t, body)
	if err != nil {
		t.Fatalf("consumeStream: %v", err)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(got.ToolCalls))
	}
	call := got.ToolCalls[0]
	if call.ID != "call_1" || call.Type != "function" || call.Function.Name != "write_file" {
		t.Errorf("identity = %+v, want id=call_1 type=function name=write_file", call)
	}
	want := `{"path":"a.txt","content":"hi"}`
	if call.Function.Arguments != want {
		t.Errorf("arguments = %q, want %q", call.Function.Arguments, want)
	}
}

// Parallel tool calls interleave, so fragments must be routed by index rather
// than appended to whichever call was most recently seen.
func TestConsumeStreamInterleavedToolCalls(t *testing.T) {
	body := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"read_file","arguments":"{\"path\""}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"list_files","arguments":"{"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"x\"}"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"}"}}]}}]}

data: [DONE]
`
	got, err := consume(t, body)
	if err != nil {
		t.Fatalf("consumeStream: %v", err)
	}
	if len(got.ToolCalls) != 2 {
		t.Fatalf("got %d tool calls, want 2", len(got.ToolCalls))
	}
	if got.ToolCalls[0].Function.Name != "read_file" || got.ToolCalls[0].Function.Arguments != `{"path":"x"}` {
		t.Errorf("call 0 = %+v", got.ToolCalls[0].Function)
	}
	if got.ToolCalls[1].Function.Name != "list_files" || got.ToolCalls[1].Function.Arguments != `{}` {
		t.Errorf("call 1 = %+v", got.ToolCalls[1].Function)
	}
}

// OpenRouter sends `:` comment lines as keepalives during a long generation.
// They must not end the stream or corrupt the content.
func TestConsumeStreamIgnoresKeepalives(t *testing.T) {
	body := `: OPENROUTER PROCESSING

data: {"choices":[{"delta":{"content":"a"}}]}

: OPENROUTER PROCESSING

data: {"choices":[{"delta":{"content":"b"}}]}

data: [DONE]
`
	got, err := consume(t, body)
	if err != nil {
		t.Fatalf("consumeStream: %v", err)
	}
	if got.Content != "ab" {
		t.Errorf("content = %q, want %q", got.Content, "ab")
	}
}

// A provider can return 200 and then report a failure inside the stream.
func TestConsumeStreamMidStreamError(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"partial"}}]}

data: {"error":{"message":"upstream provider is overloaded"}}

data: [DONE]
`
	_, err := consume(t, body)
	if err == nil {
		t.Fatal("want an error from a mid-stream error frame, got nil")
	}
	if !strings.Contains(err.Error(), "overloaded") {
		t.Errorf("error = %v, want it to carry the provider message", err)
	}
}

// An unreadable frame is skipped rather than failing a long generation.
func TestConsumeStreamSkipsUnparseableFrames(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"a"}}]}

data: {not json

data: {"choices":[{"delta":{"content":"b"}}]}

data: [DONE]
`
	got, err := consume(t, body)
	if err != nil {
		t.Fatalf("consumeStream: %v", err)
	}
	if got.Content != "ab" {
		t.Errorf("content = %q, want %q", got.Content, "ab")
	}
}

// A stream that ends without [DONE] still yields what arrived.
func TestConsumeStreamTruncated(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"partial"}}]}
`
	got, err := consume(t, body)
	if err != nil {
		t.Fatalf("consumeStream: %v", err)
	}
	if got.Content != "partial" {
		t.Errorf("content = %q, want %q", got.Content, "partial")
	}
}
