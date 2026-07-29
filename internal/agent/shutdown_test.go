package agent

import (
	"context"
	"testing"
	"time"
)

func TestShutdownWaitsForRuns(t *testing.T) {
	l := &Loop{}

	// Stand in for a run that is mid-step: cancelled, but not yet finished
	// writing its final status.
	l.wg.Add(1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if l.Shutdown(ctx) {
		t.Fatal("Shutdown reported a clean drain while a run was still going")
	}

	l.wg.Done()

	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if !l.Shutdown(ctx2) {
		t.Fatal("Shutdown did not report a clean drain after the run finished")
	}
}

func TestShutdownWithNoRuns(t *testing.T) {
	l := NewLoop(nil, nil, t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	if !l.Shutdown(ctx) {
		t.Fatal("Shutdown on an idle loop should be clean")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("idle shutdown took %s, should be immediate", elapsed)
	}
}
