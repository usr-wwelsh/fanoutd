package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// streamIdleTimeout bounds the gap between two pieces of the response, not the
// call as a whole. A total deadline cannot tell a model steadily writing a large
// file from a wedged connection: the only honest fix for one breaks the other.
// Measuring silence instead lets a slow generation run as long as it needs while
// a dead socket still fails promptly.
const streamIdleTimeout = 90 * time.Second

// idleCheckInterval is how often the watchdog looks at the clock. Polling a
// timestamp avoids the reset-and-drain dance of a shared timer.
const idleCheckInterval = streamIdleTimeout / 4

// maxSSELine caps one `data:` frame. Deltas are small, but a provider that
// batches a whole message into one frame should not blow up the scanner.
const maxSSELine = 4 << 20

// streamClient carries no total timeout on purpose — the idle watchdog owns that
// budget. The pieces below still bound everything that happens before the model
// starts producing, so a hang in connect or TLS cannot wait for the full idle
// window. One client, package-wide, so connections are reused across steps.
var streamClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
	},
}

// streamChunk is one server-sent event from a streaming completion.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string          `json:"content"`
			ToolCalls []toolCallDelta `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	// Error carries a provider failure that arrives after the 200, which is the
	// one case where a successful status still means the call failed.
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// toolCallDelta is a fragment of a tool call. Index identifies which call it
// belongs to; Arguments arrives split across frames and must be concatenated in
// order before it is valid JSON.
type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// toolCallBuilder reassembles one tool call from its fragments.
type toolCallBuilder struct {
	index int
	id    string
	kind  string
	name  string
	args  strings.Builder
}

// consumeStream reads an SSE body to completion and returns the assembled turn.
// The caller has already checked the status code. onDelta, when not nil, is
// handed each fragment of text as it arrives, so a caller can watch the reply
// being written instead of waiting for the whole of it. Cancellation arrives
// through the body itself: the watchdog cancels the request, which fails the
// next read.
func consumeStream(body *bufio.Scanner, touch func(), onDelta func(string)) (*Result, error) {
	var content strings.Builder
	builders := map[int]*toolCallBuilder{}

	for body.Scan() {
		touch()

		line := strings.TrimSpace(body.Text())
		// Blank separators and `:` comments are keepalives. They carry no data
		// but they are proof the connection is alive, which is exactly what the
		// watchdog needs — hence touch() above, before this returns to the loop.
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		payload, found := strings.CutPrefix(line, "data:")
		if !found {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// A frame we cannot read is not worth aborting a long generation
			// over; the stream as a whole still has to parse.
			continue
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return nil, fmt.Errorf("provider stream error: %s", chunk.Error.Message)
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		delta := chunk.Choices[0].Delta
		content.WriteString(delta.Content)
		if onDelta != nil && delta.Content != "" {
			onDelta(delta.Content)
		}
		for _, tc := range delta.ToolCalls {
			b := builders[tc.Index]
			if b == nil {
				b = &toolCallBuilder{index: tc.Index}
				builders[tc.Index] = b
			}
			// Identity fields arrive once, on the opening fragment.
			if tc.ID != "" {
				b.id = tc.ID
			}
			if tc.Type != "" {
				b.kind = tc.Type
			}
			if tc.Function.Name != "" {
				b.name = tc.Function.Name
			}
			b.args.WriteString(tc.Function.Arguments)
		}
	}

	if err := body.Err(); err != nil {
		return nil, err
	}

	return &Result{Content: content.String(), ToolCalls: buildToolCalls(builders)}, nil
}

// buildToolCalls flattens the accumulator back into index order. The map is
// keyed by the provider's index, which is the only ordering guarantee the
// fragments carry.
func buildToolCalls(builders map[int]*toolCallBuilder) []ToolCall {
	if len(builders) == 0 {
		return nil
	}
	ordered := make([]*toolCallBuilder, 0, len(builders))
	for _, b := range builders {
		ordered = append(ordered, b)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })

	calls := make([]ToolCall, 0, len(ordered))
	for _, b := range ordered {
		if b.name == "" {
			continue // a fragment group that never named a function is unusable
		}
		kind := b.kind
		if kind == "" {
			kind = "function"
		}
		calls = append(calls, ToolCall{
			ID:       b.id,
			Type:     kind,
			Function: FunctionCall{Name: b.name, Arguments: b.args.String()},
		})
	}
	return calls
}

// watchIdle cancels ctx once nothing has arrived for streamIdleTimeout. It
// returns the touch function the reader calls on every frame, and a flag the
// caller reads afterwards to tell a timeout apart from an ordinary cancellation.
func watchIdle(ctx context.Context, cancel context.CancelFunc) (touch func(), timedOut *atomic.Bool) {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	timedOut = &atomic.Bool{}

	go func() {
		ticker := time.NewTicker(idleCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, last.Load())) > streamIdleTimeout {
					timedOut.Store(true)
					cancel()
					return
				}
			}
		}
	}()

	return func() { last.Store(time.Now().UnixNano()) }, timedOut
}
