package agent

import (
	"context"
	"testing"

	"fanoutd/internal/models"
	"fanoutd/internal/store"
)

// A task's own ReviewOverride outranks the board's review setting — that is
// the whole point of a per-breakdown toggle. settleRun is where a finished run
// is routed to the review column or straight to finished, so it is the unit
// that has to get this right.
func TestSettleRunHonoursAPerTaskReviewOverride(t *testing.T) {
	cases := []struct {
		name           string
		boardReview    bool
		reviewOverride string
		wantColumn     string
	}{
		{"override on beats board off", false, "on", models.ColumnReview},
		{"override off beats board on", true, "off", models.ColumnFinished},
		{"no override follows board on", true, "", models.ColumnReview},
		{"no override follows board off", false, "", models.ColumnFinished},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, s := testLoop(t)
			l.SetReview(c.boardReview, "")

			task, err := s.CreateTaskFrom(store.NewTask{
				Title: "x", Goal: "x", ReviewOverride: c.reviewOverride,
			})
			if err != nil {
				t.Fatalf("CreateTaskFrom: %v", err)
			}
			if err := l.settleRun(task.ID, "done"); err != nil {
				t.Fatalf("settleRun: %v", err)
			}

			got, err := s.GetTask(task.ID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if got.Column != c.wantColumn {
				t.Errorf("column = %q, want %q", got.Column, c.wantColumn)
			}
		})
	}
}

// A breakdown's Review field is what a client actually sets; it has to survive
// the trip through BreakdownRequest into the "on"/"off"/"" a task stores.
func TestBreakdownRequestReviewOverride(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name string
		req  BreakdownRequest
		want string
	}{
		{"unset follows the board", BreakdownRequest{}, ""},
		{"explicit on", BreakdownRequest{Review: &on}, "on"},
		{"explicit off", BreakdownRequest{Review: &off}, "off"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.req.reviewOverride(); got != c.want {
				t.Errorf("reviewOverride() = %q, want %q", got, c.want)
			}
		})
	}
}

// Every task a breakdown creates has to carry the same override, including the
// fallback — a person who switched review off for one idea should not have it
// come back on just because the idea would not partition.
func TestBreakdownAppliesTheReviewOverrideToEveryTaskItCreates(t *testing.T) {
	off := false

	t.Run("group", func(t *testing.T) {
		l, _, _ := breakdownLoop(t, goodPlan)
		result, err := l.Breakdown(context.Background(), BreakdownRequest{Idea: "build a board", Review: &off})
		if err != nil {
			t.Fatalf("Breakdown: %v", err)
		}
		for _, task := range result.Tasks {
			if task.ReviewOverride != "off" {
				t.Errorf("%s review_override = %q, want off", task.Title, task.ReviewOverride)
			}
		}
	})

	t.Run("fallback", func(t *testing.T) {
		l, _, _ := breakdownLoop(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := l.Breakdown(ctx, BreakdownRequest{Idea: "build a board", Review: &off})
		if err != nil {
			t.Fatalf("Breakdown: %v", err)
		}
		if result.Fallback == "" {
			t.Fatal("want the single-task floor")
		}
		if result.Tasks[0].ReviewOverride != "off" {
			t.Errorf("fallback review_override = %q, want off", result.Tasks[0].ReviewOverride)
		}
	})
}
