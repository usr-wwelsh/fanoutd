package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"fanoutd/internal/agent"
	"fanoutd/internal/config"
	"fanoutd/internal/llm"
	"fanoutd/internal/server"
	"fanoutd/internal/store"
)

const (
	// httpDrainTimeout bounds the wait for in-flight requests. They are short;
	// the long-poll cases are the client's own retry loop.
	httpDrainTimeout = 5 * time.Second
	// loopDrainTimeout bounds the wait for agent runs to record their status. A
	// cancelled run stops at its next step boundary, but it may first have to
	// finish an OpenRouter call already in flight.
	loopDrainTimeout = 30 * time.Second
)

func main() {
	cfg := config.Load()

	// Resolve before anything is opened or created. A provider that cannot be
	// built is a configuration error, and it should read as one at startup
	// rather than as a failed task six steps into the first run.
	client, err := llm.Resolve(cfg.Provider, cfg.BaseURL, cfg.APIKey, cfg.Model)
	if err != nil {
		log.Fatalf("provider: %v", err)
	}

	// The database driver will not create the directory holding its file, and
	// on a fresh machine the data directory does not exist yet.
	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0o755); err != nil {
		log.Fatalf("failed to create data directory: %v", err)
	}
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		log.Fatalf("failed to create output directory: %v", err)
	}
	if cfg.EnvFile != "" {
		log.Printf("settings from %s\n", cfg.EnvFile)
	}
	log.Printf("database %s\n", cfg.DatabasePath)
	log.Printf("workspaces %s\n", cfg.OutputDir)

	// Write the resolved settings back, so everything downstream reports what is
	// actually in force rather than what was typed. A preset supplies the model
	// when the operator names none, and the picker's "default" has to agree with
	// the model the loop will really use.
	cfg.Provider, cfg.Model, cfg.BaseURL = client.Provider.Name, client.Model, client.BaseURL
	log.Printf("provider %s (%s) model %s\n", cfg.Provider, cfg.BaseURL, cfg.Model)

	s, err := store.NewStore(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}

	// Runs live in this process's memory, so anything still marked running got
	// there from a previous process that did not exit cleanly.
	if n, err := s.ReclaimRunningTasks(); err != nil {
		log.Printf("could not reclaim interrupted tasks: %v", err)
	} else if n > 0 {
		log.Printf("reclaimed %d task(s) left running by an earlier process\n", n)
	}

	loop := agent.NewLoop(s, client, cfg.OutputDir)
	loop.SetMaxParallel(cfg.MaxParallel)
	loop.SetMaxSteps(cfg.MaxSteps)

	if cfg.Review {
		loop.SetReview(true, cfg.ReviewModel)
		if cfg.ReviewModel == "" {
			log.Println("review enabled on each task's own model — set FANOUT_REVIEW_MODEL to a different one, since a model reviewing its own output agrees with it")
		} else {
			log.Printf("review enabled (%s)\n", cfg.ReviewModel)
		}
	}

	// A sandbox that cannot be built is not degraded into an unsandboxed shell:
	// the tool is simply never offered, and agents stay file-only.
	if cfg.Shell {
		sb, err := agent.NewSandbox(agent.SandboxConfig{
			Network:   cfg.ShellNet,
			Timeout:   cfg.ShellTimeout,
			MemoryMax: cfg.ShellMemory,
			TasksMax:  cfg.ShellTasks,
			CPUQuota:  cfg.ShellCPU,
			MaxExec:   cfg.ShellMaxExec,
			ROBind:    cfg.ShellROBind,
			StateDir:  cfg.SandboxDir,
		})
		if err != nil {
			log.Printf("shell commands disabled: %v\n", err)
		} else {
			loop.SetSandbox(sb)
			log.Printf("shell commands enabled (%s)\n", sb.Describe())
			if n, err := sb.ReapBuildDirs(liveTaskIDs(s)); err != nil {
				log.Printf("could not reap orphaned build directories: %v\n", err)
			} else if n > 0 {
				log.Printf("reaped %d orphaned build director(ies)\n", n)
			}
		}
	}
	// After the sandbox, so a swept review can run what it is judging rather than
	// only read it. A verdict is delivered by the goroutine that ran the task, so
	// work parked in review by a process that went away is owed one by this one.
	if n := loop.ReviewParked(context.Background()); n > 0 {
		log.Printf("reviewing %d run(s) left awaiting a verdict by an earlier process\n", n)
	} else if !cfg.Review {
		// Nothing here will judge them, and nothing lists them either: they are
		// filed as done, which is what `fanout blocked` skips. Saying so once is
		// the difference between work waiting and work lost.
		if parked, err := s.TasksAwaitingReview(); err == nil && len(parked) > 0 {
			log.Printf("%d run(s) are parked in review with FANOUT_REVIEW off; turn it on to have them judged, or move them on the board\n", len(parked))
		}
	}

	srv := server.New(s, loop, client, cfg, ui())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := srv.Start(cfg.Port); err != nil {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-stop
	log.Println("shutting down...")

	// Close the door before draining, so a late POST /start cannot open a run
	// that nothing is left to finish.
	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), httpDrainTimeout)
	defer cancelHTTP()
	if err := srv.Shutdown(httpCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}

	loopCtx, cancelLoop := context.WithTimeout(context.Background(), loopDrainTimeout)
	defer cancelLoop()
	if !loop.Shutdown(loopCtx) {
		log.Printf("gave up waiting for agent runs after %s; they will be reclaimed at the next start", loopDrainTimeout)
	}
	log.Println("stopped")
}

// liveTaskIDs is the set a build directory has to belong to in order to survive
// the reap. A store that cannot be read returns nothing, and nothing is reaped —
// deleting scratch is not worth doing on a guess.
func liveTaskIDs(s *store.Store) map[string]bool {
	tasks, err := s.ListTasks()
	if err != nil {
		log.Printf("could not list tasks to reap build directories: %v\n", err)
		return nil
	}
	live := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		live[t.ID] = true
	}
	return live
}
