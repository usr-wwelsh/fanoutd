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
	"fanoutd/internal/openrouter"
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
	if cfg.OpenRouterKey == "" {
		log.Fatal("OPENROUTER_API_KEY is required. Set it in the env file or OPENROUTER_API_KEY env var")
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

	client := openrouter.NewClient(cfg.OpenRouterKey, cfg.OpenRouterModel, cfg.BaseURL)
	loop := agent.NewLoop(s, client, cfg.OutputDir)
	loop.SetMaxParallel(cfg.MaxParallel)

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
