package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Free endpoints fail transiently far more often than they fail for good: a
// 502 from an overloaded upstream, a connection dropped mid-handshake. A run
// that dies to either loses every step it paid for, so the client re-asks
// before giving up.

// flaky serves status n times, then streams a completion.
type flaky struct {
	mu     sync.Mutex
	status int
	left   int
	drop   bool
	calls  int
}

func (f *flaky) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	fail := f.left > 0
	if fail {
		f.left--
	}
	drop := f.drop && fail
	f.mu.Unlock()

	if fail {
		if drop {
			hj, ok := w.(http.Hijacker)
			if !ok {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			conn, _, _ := hj.Hijack()
			if conn != nil {
				conn.Close()
			}
			return
		}
		w.WriteHeader(f.status)
		fmt.Fprint(w, `{"error":{"message":"upstream error"}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, okStream("hello"))
}

func (f *flaky) seen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestChatSurvivesATransientServerError(t *testing.T) {
	skipWaits(t)
	f := &flaky{status: http.StatusBadGateway, left: 1}
	srv := httptest.NewServer(f)
	defer srv.Close()

	c := NewClient(presets["openrouter"], "k", "m", srv.URL)
	res, err := c.Chat(context.Background(), []MsgBlock{{Role: "user", Content: "hi"}}, ChatOptions{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Content != "hello" {
		t.Errorf("content = %q", res.Content)
	}
	if f.seen() != 2 {
		t.Errorf("made %d requests, want 2 (one failed, one served)", f.seen())
	}
}

func TestChatSurvivesADroppedConnection(t *testing.T) {
	skipWaits(t)
	f := &flaky{status: http.StatusBadGateway, left: 1, drop: true}
	srv := httptest.NewServer(f)
	defer srv.Close()

	c := NewClient(presets["openrouter"], "k", "m", srv.URL)
	res, err := c.Chat(context.Background(), []MsgBlock{{Role: "user", Content: "hi"}}, ChatOptions{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Content != "hello" {
		t.Errorf("content = %q", res.Content)
	}
	if f.seen() != 2 {
		t.Errorf("made %d requests, want 2", f.seen())
	}
}

func TestChatGivesUpOnAPermanentServerError(t *testing.T) {
	skipWaits(t)
	f := &flaky{status: http.StatusServiceUnavailable, left: 1000}
	srv := httptest.NewServer(f)
	defer srv.Close()

	c := NewClient(presets["openrouter"], "k", "m", srv.URL)
	_, err := c.Chat(context.Background(), []MsgBlock{{Role: "user", Content: "hi"}}, ChatOptions{})
	if err == nil {
		t.Fatal("a permanent server error returned success")
	}
	if !strings.Contains(err.Error(), fmt.Sprint(http.StatusServiceUnavailable)) {
		t.Errorf("error %q does not name the status", err)
	}
	if f.seen() != transientRetries+1 {
		t.Errorf("made %d requests, want %d", f.seen(), transientRetries+1)
	}
}

// A cancelled task must come back at once, not after one more attempt.
func TestTransientRetryHonoursCancellation(t *testing.T) {
	skipWaits(t)
	f := &flaky{status: http.StatusBadGateway, left: 1000}
	srv := httptest.NewServer(f)
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
		t.Error("the retry outlived its context")
	}
}
