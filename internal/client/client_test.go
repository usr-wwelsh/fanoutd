package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestErrorCarriesStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server errors with http.Error, so bodies are plain text.
		http.Error(w, "task not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "").GetTask(context.Background(), "nope")
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("got %T, want *Error", err)
	}
	if !apiErr.NotFound() {
		t.Errorf("status = %d", apiErr.Status)
	}
	if !strings.Contains(apiErr.Error(), "task not found") {
		t.Errorf("error = %q, want the server's message", apiErr)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := map[int]func(*Error) bool{
		http.StatusNotFound:     (*Error).NotFound,
		http.StatusUnauthorized: (*Error).Unauthorized,
		http.StatusConflict:     (*Error).Conflict,
	}
	for status, pred := range cases {
		e := &Error{Status: status}
		if !pred(e) {
			t.Errorf("status %d not classified", status)
		}
	}
}

func TestTokenIsSentAsBearer(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Write([]byte("[]"))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "secret").ListTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer secret" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestNoTokenSendsNoHeader(t *testing.T) {
	var seen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, seen = r.Header["Authorization"]
		w.Write([]byte("[]"))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "").ListTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen {
		t.Error("an empty token must not produce an Authorization header")
	}
}

// Raw returns bytes rather than JSON, so it takes its own path through the
// client and needs its own coverage.
func TestRawEscapesThePathQuery(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Query().Get("path")
		w.Write([]byte("<html>"))
	}))
	defer srv.Close()

	data, err := New(srv.URL, "").Raw(context.Background(), "abc", "src/a b&c.html")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "src/a b&c.html" {
		t.Errorf("path = %q — the query was not escaped correctly", gotPath)
	}
	if string(data) != "<html>" {
		t.Errorf("body = %q", data)
	}
}

func TestRawReportsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "file not found", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "").Raw(context.Background(), "abc", "gone.txt"); err == nil {
		t.Fatal("expected an error")
	}
}

// The server takes Model as *string because "" means "fall back to the
// default". An unset --model must therefore be omitted from the body entirely,
// which is what makes continue and retry inherit the old task's model.
func TestFollowupOmitsAnUnsetModel(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		body = string(buf[:n])
		w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	if _, err := c.RetryTask(context.Background(), "abc", Followup{Start: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "model") {
		t.Errorf("body %q should not carry a model key", body)
	}

	model := "openai/gpt-4"
	if _, err := c.RetryTask(context.Background(), "abc", Followup{Model: &model}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "openai/gpt-4") {
		t.Errorf("body %q should carry the model", body)
	}
}

func TestBaseURLTrailingSlashIsTrimmed(t *testing.T) {
	if got := New("http://x.example/", "").BaseURL(); got != "http://x.example" {
		t.Errorf("got %q", got)
	}
}
