package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Client speaks OpenAI chat/completions over SSE. It is the implementation
// almost every provider is reached through — vendors and local servers alike —
// so its base URL and key are what a provider record configures, not code.
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

var _ API = (*Client)(nil)

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
		baseURL = "https://llm.ai/api/v1"
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
const rateLimitRetries = 5

// rateLimitBackoff is the delay before the first retry when the refusal came
// with no hint about when to come back; it doubles each time.
const rateLimitBackoff = time.Second

// rateLimitMaxWait caps a single wait. The free tier's limit is per minute and
// resets on a wall-clock boundary, so a hint just under a minute out is normal
// and worth honouring — but a provider naming an hour is telling us to stop, not
// to sleep through the run.
const rateLimitMaxWait = 75 * time.Second

// rateLimitJitter spreads the retries of agents that were refused together. A
// breakdown runs subtasks in parallel against one key, so they hit the limit in
// the same second and are handed the same reset instant; without this they all
// wake at once and the first one through takes the window again.
const rateLimitJitter = 2 * time.Second

// attemptResult is one round trip. A non-2xx carries its body so the caller can
// build the error message; a 200 carries the assembled turn.
type attemptResult struct {
	result     *Result
	status     int
	retryAfter string
	// resetAt is the X-RateLimit-Reset header: the instant the current window
	// ends, which is the only useful hint OpenRouter gives on a free-tier 429.
	resetAt string
	body    string
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
		if got.status != http.StatusTooManyRequests {
			return nil, fmt.Errorf("openrouter error %d: %s", got.status, got.body)
		}
		if attempt >= rateLimitRetries {
			// Say plainly that waiting was tried, so the trace does not read as a
			// run that gave up on the first refusal.
			return nil, fmt.Errorf("still rate limited after %d retries: %s", attempt, got.body)
		}

		wait := delay
		if hinted, ok := retryHint(got); ok {
			wait = hinted
		}
		if err := waitFor(ctx, wait+jitter()); err != nil {
			return nil, err
		}
		delay *= 2
	}
}

// retryHint reads when the provider says to come back, in the three places it
// might say so. Retry-After is the standard one and nobody sends it here;
// X-RateLimit-Reset is what OpenRouter actually sets; and on a free-tier refusal
// the headers are also copied into the error body, which is the only copy that
// survives some proxies. Without any of them the caller falls back to doubling a
// second, which gives up about fifteen seconds into a window that resets on the
// minute — the whole reason a routine rate limit was killing runs.
func retryHint(got attemptResult) (time.Duration, bool) {
	if d, ok := parseRetryAfter(got.retryAfter); ok {
		return d, true
	}
	if d, ok := parseResetAt(got.resetAt); ok {
		return d, true
	}
	return parseResetAt(resetFromBody(got.body))
}

// resetFromBody digs the reset out of the error envelope OpenRouter returns:
// {"error":{"metadata":{"headers":{"X-RateLimit-Reset":"1785537420000"}}}}.
func resetFromBody(body string) string {
	if body == "" {
		return ""
	}
	var parsed struct {
		Error struct {
			Metadata struct {
				Headers map[string]string `json:"headers"`
			} `json:"metadata"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return ""
	}
	for k, v := range parsed.Error.Metadata.Headers {
		if strings.EqualFold(k, "X-RateLimit-Reset") {
			return v
		}
	}
	return ""
}

// parseResetAt reads a rate-limit reset. The value is an instant, not a delay,
// and its unit is left to the provider — so it is read as milliseconds, seconds,
// or a plain delay according to its magnitude, which is the only way to tell an
// epoch from a duration.
func parseResetAt(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}

	var d time.Duration
	switch {
	case n > 1e12: // epoch milliseconds
		d = time.Until(time.UnixMilli(n))
	case n > 1e9: // epoch seconds
		d = time.Until(time.Unix(n, 0))
	default: // a delay in seconds
		d = time.Duration(n) * time.Second
	}
	return clampWait(d)
}

// clampWait rejects a hint that has already passed or reaches past the point of
// waiting. A window that closed while the response was in flight is not an
// error: the caller retries immediately, which is what a zero-length wait means.
func clampWait(d time.Duration) (time.Duration, bool) {
	if d > rateLimitMaxWait {
		return 0, false
	}
	if d < 0 {
		d = 0
	}
	return d, true
}

// jitter is a small random spread, never negative, so simultaneous waiters do
// not resume in lockstep. It is a variable so a test measuring which wait was
// chosen is not measuring the spread on top of it.
var jitter = func() time.Duration {
	return time.Duration(rand.Int63n(int64(rateLimitJitter)))
}

// waitFor pauses between rate-limit retries, returning early if the run is
// cancelled — a cancelled task must not have to sit out the backoff first. It is
// a variable so a test can assert which delays were chosen without spending them
// in real time; the schedule is minutes long by design and no suite should wait
// it out.
var waitFor = func(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
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
			resetAt:    resp.Header.Get("X-RateLimit-Reset"),
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
	return clampWait(d)
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
