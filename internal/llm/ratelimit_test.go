package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The refusal a free-tier key actually gets. There is no Retry-After header on
// it: the only hint is X-RateLimit-Reset, in epoch milliseconds, and OpenRouter
// copies it into the error body as well as the headers.
func rateLimitBody(resetAt time.Time) string {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": "Rate limit exceeded: free-models-per-min. ",
			"code":    429,
			"metadata": map[string]any{
				"headers": map[string]string{
					"X-RateLimit-Limit":     "20",
					"X-RateLimit-Remaining": "0",
					"X-RateLimit-Reset":     fmt.Sprint(resetAt.UnixMilli()),
				},
				"limit_source": "openrouter_free_tier_per_minute",
			},
		},
	})
	return string(body)
}

func okStream(content string) string {
	frame, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": content}}},
	})
	return fmt.Sprintf("data: %s\n\ndata: [DONE]\n\n", frame)
}

// limiter refuses the first n requests with a 429 and serves the rest.
type limiter struct {
	mu sync.Mutex
	// refusals is how many 429s are left to serve.
	refusals int
	// sendHeader controls whether the reset is advertised as a response header
	// as well as in the body.
	sendHeader bool
	// noHint refuses without saying when the window reopens, which is what the
	// exponential fallback exists for.
	noHint bool
	reset  time.Time
	calls  int
}

func (l *limiter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	l.calls++
	refuse := l.refusals > 0
	if refuse {
		l.refusals--
	}
	l.mu.Unlock()

	if refuse {
		if l.noHint {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"Rate limit exceeded","code":429}}`)
			return
		}
		if l.sendHeader {
			w.Header().Set("X-RateLimit-Reset", fmt.Sprint(l.reset.UnixMilli()))
		}
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, rateLimitBody(l.reset))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, okStream("hello"))
}

func (l *limiter) seen() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// noJitter pins the random spread to zero, so a test can measure which wait was
// chosen rather than the spread laid on top of it.
func noJitter(t *testing.T) {
	t.Helper()
	prev := jitter
	jitter = func() time.Duration { return 0 }
	t.Cleanup(func() { jitter = prev })
}

// recordedWaits collects the delays the retry loop chose and returns from each
// of them at once. The schedule runs to minutes by design, so a test asserts
// what was asked for rather than sitting through it.
type recordedWaits struct {
	mu   sync.Mutex
	list []time.Duration
}

func (r *recordedWaits) all() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.list...)
}

func skipWaits(t *testing.T) *recordedWaits {
	t.Helper()
	noJitter(t)
	rec := &recordedWaits{}
	prev := waitFor
	waitFor = func(ctx context.Context, d time.Duration) error {
		rec.mu.Lock()
		rec.list = append(rec.list, d)
		rec.mu.Unlock()
		return ctx.Err()
	}
	t.Cleanup(func() { waitFor = prev })
	return rec
}

func TestChatWaitsOutARateLimit(t *testing.T) {
	waits := skipWaits(t)
	lim := &limiter{refusals: 2, reset: time.Now().Add(30 * time.Second)}
	srv := httptest.NewServer(lim)
	defer srv.Close()

	c := NewClient(presets["openrouter"], "k", "m", srv.URL)
	res, err := c.Chat(context.Background(), []MsgBlock{{Role: "user", Content: "hi"}}, ChatOptions{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Content != "hello" {
		t.Errorf("content = %q", res.Content)
	}
	if lim.seen() != 3 {
		t.Errorf("made %d requests, want 3 (two refused, one served)", lim.seen())
	}
	// Both waits came from the reset in the body, not from the backoff, whose
	// first two delays would have been a second and two seconds.
	got := waits.all()
	if len(got) != 2 {
		t.Fatalf("waited %d times, want 2", len(got))
	}
	for i, d := range got {
		if d < 25*time.Second || d > 30*time.Second {
			t.Errorf("wait %d was %s, want the hinted reset of about 30s", i, d)
		}
	}
}

// The header is the documented place to look; the body copy is the fallback.
// Either alone has to be enough.
func TestRateLimitResetIsReadFromTheHeaderToo(t *testing.T) {
	waits := skipWaits(t)
	lim := &limiter{refusals: 1, sendHeader: true, reset: time.Now().Add(30 * time.Second)}
	srv := httptest.NewServer(lim)
	defer srv.Close()

	c := NewClient(presets["openrouter"], "k", "m", srv.URL)
	if _, err := c.Chat(context.Background(), []MsgBlock{{Role: "user", Content: "hi"}}, ChatOptions{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if lim.seen() != 2 {
		t.Errorf("made %d requests, want 2", lim.seen())
	}
	got := waits.all()
	if len(got) != 1 {
		t.Fatalf("waited %d times, want 1", len(got))
	}
	if got[0] < 25*time.Second || got[0] > 30*time.Second {
		t.Errorf("waited %s, want the reset from the header", got[0])
	}
}

// Without a hint the loop falls back to doubling a second, which is the only
// thing standing between a silent 429 and a hot retry loop.
func TestUnhintedRateLimitBacksOffExponentially(t *testing.T) {
	waits := skipWaits(t)
	lim := &limiter{refusals: 1000, noHint: true}
	srv := httptest.NewServer(lim)
	defer srv.Close()

	c := NewClient(presets["openrouter"], "k", "m", srv.URL)
	if _, err := c.Chat(context.Background(), []MsgBlock{{Role: "user", Content: "hi"}}, ChatOptions{}); err == nil {
		t.Fatal("a permanent rate limit returned success")
	}

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	got := waits.all()
	if len(got) != len(want) {
		t.Fatalf("waited %d times, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("wait %d was %s, want %s", i, got[i], want[i])
		}
	}
}

// A limit that never lifts still has to end, and say what it was.
func TestChatGivesUpOnAPermanentRateLimit(t *testing.T) {
	skipWaits(t)
	lim := &limiter{refusals: 1000, reset: time.Now().Add(30 * time.Second)}
	srv := httptest.NewServer(lim)
	defer srv.Close()

	c := NewClient(presets["openrouter"], "k", "m", srv.URL)
	_, err := c.Chat(context.Background(), []MsgBlock{{Role: "user", Content: "hi"}}, ChatOptions{})
	if err == nil {
		t.Fatal("a permanent rate limit returned success")
	}
	if !strings.Contains(err.Error(), "still rate limited") {
		t.Errorf("error %q does not say the wait was tried", err)
	}
	if lim.seen() != rateLimitRetries+1 {
		t.Errorf("made %d requests, want %d", lim.seen(), rateLimitRetries+1)
	}
}

// Cancelling a task must not have to sit through the wait first.
func TestRateLimitWaitHonoursCancellation(t *testing.T) {
	lim := &limiter{refusals: 1000, reset: time.Now().Add(30 * time.Second)}
	srv := httptest.NewServer(lim)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := NewClient(presets["openrouter"], "k", "m", srv.URL)

	done := make(chan error, 1)
	go func() {
		_, err := c.Chat(ctx, []MsgBlock{{Role: "user", Content: "hi"}}, ChatOptions{})
		done <- err
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled call returned success")
		}
	case <-time.After(3 * time.Second):
		t.Error("the wait outlived its context")
	}
}

func TestParseResetAt(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		in   string
		want time.Duration
		ok   bool
	}{
		{"epoch milliseconds", fmt.Sprint(now.Add(30 * time.Second).UnixMilli()), 30 * time.Second, true},
		{"epoch seconds", fmt.Sprint(now.Add(30 * time.Second).Unix()), 30 * time.Second, true},
		{"a plain delay", "12", 12 * time.Second, true},
		// The window closed while the response was in flight: retry at once
		// rather than treating a stale hint as no hint.
		{"already past", fmt.Sprint(now.Add(-5 * time.Second).UnixMilli()), 0, true},
		{"too far out", fmt.Sprint(now.Add(time.Hour).UnixMilli()), 0, false},
		{"empty", "", 0, false},
		{"not a number", "soon", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseResetAt(tt.in)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			// Wall-clock arithmetic, so compare loosely.
			if ok && (got < tt.want-time.Second || got > tt.want+time.Second) {
				t.Errorf("got %s, want about %s", got, tt.want)
			}
		})
	}
}

func TestResetFromBody(t *testing.T) {
	at := time.Now().Add(time.Minute)
	if got := resetFromBody(rateLimitBody(at)); got != fmt.Sprint(at.UnixMilli()) {
		t.Errorf("resetFromBody = %q, want %d", got, at.UnixMilli())
	}
	for _, body := range []string{"", "not json", `{"error":{}}`, `{"error":{"metadata":{}}}`} {
		if got := resetFromBody(body); got != "" {
			t.Errorf("resetFromBody(%q) = %q, want empty", body, got)
		}
	}
}
