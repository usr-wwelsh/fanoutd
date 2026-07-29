package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"fanoutd/internal/client"
	"fanoutd/internal/models"
)

func taskServer(t *testing.T, tasks []models.Task) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tasks" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(tasks)
	}))
	t.Cleanup(srv.Close)
	return client.New(srv.URL, "")
}

func TestResolveID(t *testing.T) {
	tasks := []models.Task{
		{ID: "c762903a-1111-4444-8888-000000000001", Title: "Tetris clone"},
		{ID: "c762aaaa-2222-4444-8888-000000000002", Title: "Tetris scoring"},
		{ID: "88d9af4b-3333-4444-8888-000000000003", Title: "Research digest MVP"},
	}
	c := taskServer(t, tasks)
	ctx := context.Background()

	t.Run("unique prefix", func(t *testing.T) {
		got, err := resolveID(ctx, c, "88d9")
		if err != nil {
			t.Fatal(err)
		}
		if got != tasks[2].ID {
			t.Errorf("got %s", got)
		}
	})

	t.Run("full id", func(t *testing.T) {
		got, err := resolveID(ctx, c, tasks[0].ID)
		if err != nil || got != tasks[0].ID {
			t.Fatalf("got (%s, %v)", got, err)
		}
	})

	t.Run("ambiguous prefix names the candidates", func(t *testing.T) {
		_, err := resolveID(ctx, c, "c762")
		if err == nil {
			t.Fatal("expected an ambiguity error")
		}
		for _, want := range []string{"c762903", "c762aaa", "longer prefix"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q is missing %q", err, want)
			}
		}
	})

	t.Run("title fallback", func(t *testing.T) {
		got, err := resolveID(ctx, c, "digest")
		if err != nil {
			t.Fatal(err)
		}
		if got != tasks[2].ID {
			t.Errorf("got %s", got)
		}
	})

	t.Run("ambiguous title", func(t *testing.T) {
		if _, err := resolveID(ctx, c, "Tetris"); err == nil {
			t.Fatal("expected an ambiguity error")
		}
	})

	t.Run("no match", func(t *testing.T) {
		if _, err := resolveID(ctx, c, "zzzz"); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("empty", func(t *testing.T) {
		if _, err := resolveID(ctx, c, "  "); err == nil {
			t.Fatal("expected an error")
		}
	})
}

// An id prefix must win over a title that happens to contain it, so a task
// titled after another task's id cannot shadow the real one.
func TestResolveIDPrefixBeatsTitle(t *testing.T) {
	tasks := []models.Task{
		{ID: "abc12340-0000-0000-0000-000000000001", Title: "Board"},
		{ID: "ffffffff-0000-0000-0000-000000000002", Title: "notes on abc1234"},
	}
	c := taskServer(t, tasks)
	got, err := resolveID(context.Background(), c, "abc1234")
	if err != nil {
		t.Fatal(err)
	}
	if got != tasks[0].ID {
		t.Errorf("got %s, want the id match", got)
	}
}

func TestResolveGroupID(t *testing.T) {
	tasks := []models.Task{
		{ID: "aaaa0001-0000-0000-0000-000000000001", Title: "schema", GroupID: "g1110000-0000-0000-0000-0000000000g1"},
		{ID: "aaaa0002-0000-0000-0000-000000000002", Title: "impl", GroupID: "g1110000-0000-0000-0000-0000000000g1"},
		{ID: "aaaa0003-0000-0000-0000-000000000003", Title: "digest", GroupID: "g2220000-0000-0000-0000-0000000000g2"},
		// A task outside any breakdown is not a candidate.
		{ID: "aaaa0004-0000-0000-0000-000000000004", Title: "Tetris clone"},
	}
	c := taskServer(t, tasks)
	ctx := context.Background()

	t.Run("prefix", func(t *testing.T) {
		got, err := resolveGroupID(ctx, c, "g111")
		if err != nil || got != tasks[0].GroupID {
			t.Fatalf("got (%s, %v)", got, err)
		}
	})

	// A group is named by any of its subtasks, since that is what a user has in
	// front of them - the group id itself appears nowhere but `fanout show`.
	t.Run("subtask title", func(t *testing.T) {
		got, err := resolveGroupID(ctx, c, "impl")
		if err != nil || got != tasks[0].GroupID {
			t.Fatalf("got (%s, %v)", got, err)
		}
	})

	t.Run("no match", func(t *testing.T) {
		if _, err := resolveGroupID(ctx, c, "Tetris"); err == nil {
			t.Error("a task outside every group resolved to one")
		}
	})

	t.Run("ambiguous prefix lists the breakdowns", func(t *testing.T) {
		_, err := resolveGroupID(ctx, c, "g")
		if err == nil {
			t.Fatal("expected an ambiguity error")
		}
		if !strings.Contains(err.Error(), "2 subtasks") || !strings.Contains(err.Error(), "longer prefix") {
			t.Errorf("error %q should size each breakdown", err)
		}
	})
}

func TestShortID(t *testing.T) {
	if got := shortID("c762903a-1111"); got != "c762903" {
		t.Errorf("got %q", got)
	}
	if got := shortID("abc"); got != "abc" {
		t.Errorf("short ids pass through unchanged, got %q", got)
	}
}
