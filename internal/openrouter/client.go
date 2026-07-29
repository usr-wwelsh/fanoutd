package openrouter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Client struct {
	APIKey  string
	Model   string
	BaseURL string

	mu sync.Mutex
	// noJSONMode records the models whose provider rejected response_format, so
	// later ForceJSON requests for them fall straight back to tools.
	noJSONMode map[string]bool
	// models caches the catalog; it changes far slower than a session lasts.
	models      []Model
	modelsAt    time.Time
	modelsError error
}

type MsgBlock struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall is a native tool call returned by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name string `json:"name"`
	// Arguments is a JSON object encoded as a string, per the OpenAI schema.
	Arguments string `json:"arguments"`
}

// Tool advertises a callable function to the model.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// Result is one model turn: free text, native tool calls, or both.
type Result struct {
	Content   string
	ToolCalls []ToolCall
}

// ChatOptions selects how the model should shape its reply. Tools and ForceJSON
// are mutually exclusive: several providers suppress tool calls when a JSON
// response format is set, so ForceJSON is meant as a fallback, not an addition.
type ChatOptions struct {
	Tools []Tool
	// ForceJSON constrains the reply to a single JSON object via response_format.
	ForceJSON bool
	// Model overrides the client default for this request. Empty uses the default.
	Model string
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []MsgBlock      `json:"messages"`
	Tools          []Tool          `json:"tools,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Stream         bool            `json:"stream"`
}

func NewClient(apiKey, model, baseURL string) *Client {
	if model == "" {
		model = "inclusionai/ling-3.0-flash:free"
	}
	if baseURL == "" {
		baseURL = "https://openrouter.ai/api/v1"
	}
	return &Client{
		APIKey:     apiKey,
		Model:      model,
		BaseURL:    strings.TrimSuffix(baseURL, "/"),
		noJSONMode: map[string]bool{},
	}
}

// Chat sends one completion request. Advertising tools lets the model reply with
// native tool calls instead of hand-rolled JSON in the message body.
//
// Not every model supports response_format. When one rejects it, the request is
// retried with tools so an unsupported feature cannot abort the run.
func (c *Client) Chat(ctx context.Context, messages []MsgBlock, opts ChatOptions) (*Result, error) {
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = c.Model
	}
	jsonMode := opts.ForceJSON && !c.jsonModeRejected(model)
	result, err := c.send(ctx, messages, opts, jsonMode)
	if err != nil && jsonMode && isUnsupportedFormat(err) {
		c.rejectJSONMode(model)
		return c.send(ctx, messages, opts, false)
	}
	return result, err
}

func (c *Client) jsonModeRejected(model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.noJSONMode[model]
}

func (c *Client) rejectJSONMode(model string) {
	c.mu.Lock()
	c.noJSONMode[model] = true
	c.mu.Unlock()
}

// isUnsupportedFormat spots a provider refusing response_format specifically, as
// opposed to a bad request we should surface.
func isUnsupportedFormat(err error) bool {
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "400") {
		return false
	}
	return strings.Contains(msg, "structured-outputs") ||
		strings.Contains(msg, "structured outputs") ||
		strings.Contains(msg, "response_format")
}

// rateLimitRetries is how many extra attempts a 429 gets, on top of the first.
const rateLimitRetries = 4

// rateLimitBackoff is the delay before the first retry; it doubles each time.
const rateLimitBackoff = time.Second

// attemptResult is one round trip. A non-2xx carries its body so the caller can
// build the error message; a 200 carries the assembled turn.
type attemptResult struct {
	result     *Result
	status     int
	retryAfter string
	body       string
}

// post sends the request body, retrying a 429 with exponential backoff. Rate
// limiting is routine on free models and shorter-lived than a whole run, so it
// must not abort the task on the first refusal.
func (c *Client) post(ctx context.Context, body []byte) (*Result, error) {
	delay := rateLimitBackoff

	for attempt := 0; ; attempt++ {
		got, err := c.attempt(ctx, body)
		if err != nil {
			return nil, err
		}
		if got.status == http.StatusOK {
			return got.result, nil
		}
		if got.status != http.StatusTooManyRequests || attempt >= rateLimitRetries {
			return nil, fmt.Errorf("openrouter error %d: %s", got.status, got.body)
		}

		wait := delay
		if hinted, ok := parseRetryAfter(got.retryAfter); ok {
			wait = hinted
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		delay *= 2
	}
}

// attempt runs one streaming request. The response is consumed as it arrives so
// that a long generation is bounded by silence rather than by total elapsed
// time — see streamIdleTimeout.
func (c *Client) attempt(ctx context.Context, body []byte) (attemptResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return attemptResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("HTTP-Referer", "fanoutd")
	req.Header.Set("X-Title", "fanoutd")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	touch, timedOut := watchIdle(ctx, cancel)

	resp, err := streamClient.Do(req)
	if err != nil {
		return attemptResult{}, describeIdle(err, timedOut)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return attemptResult{
			status:     resp.StatusCode,
			retryAfter: resp.Header.Get("Retry-After"),
			body:       strings.TrimSpace(string(errBody)),
		}, nil
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELine)

	result, err := consumeStream(scanner, touch)
	if err != nil {
		return attemptResult{}, describeIdle(err, timedOut)
	}
	return attemptResult{result: result, status: http.StatusOK}, nil
}

// describeIdle replaces the generic cancellation error with the real reason when
// the watchdog is what fired, so a stalled provider does not read as a bug in
// the caller's context handling.
func describeIdle(err error, timedOut *atomic.Bool) error {
	if timedOut != nil && timedOut.Load() {
		return fmt.Errorf("model sent nothing for %s", streamIdleTimeout)
	}
	return err
}

// parseRetryAfter reads the header in either of its forms - delay seconds or an
// HTTP date - and ignores values too far out to be worth waiting on.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(secs) * time.Second
	} else if at, err := http.ParseTime(v); err == nil {
		d = time.Until(at)
	} else {
		return 0, false
	}
	if d <= 0 || d > time.Minute {
		return 0, false
	}
	return d, true
}

func (c *Client) send(ctx context.Context, messages []MsgBlock, opts ChatOptions, jsonMode bool) (*Result, error) {
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = c.Model
	}
	reqBody := chatRequest{
		Model:    model,
		Messages: messages,
		Tools:    opts.Tools,
	}
	if jsonMode {
		reqBody.Tools = nil
		reqBody.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	reqBody.Stream = true
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	result, err := c.post(ctx, body)
	if err != nil {
		return nil, err
	}
	if result.Content == "" && len(result.ToolCalls) == 0 {
		return nil, fmt.Errorf("no content returned from openrouter")
	}
	return result, nil
}
