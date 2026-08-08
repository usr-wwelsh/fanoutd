package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"fanoutd/internal/agent"
	"fanoutd/internal/config"
	"fanoutd/internal/models"
	"fanoutd/internal/llm"
	"fanoutd/internal/store"
)

// breakdownBudget bounds a breakdown end to end: two model calls plus the rows.
// It doubles as the server's write deadline, since it is the longest any
// endpoint legitimately takes.
const breakdownBudget = 5 * time.Minute

type Server struct {
	store *store.Store
	loop  *agent.Loop
	ui    fs.FS

	mu   sync.Mutex
	http *http.Server
	// client and cfg are behind the mutex because the settings endpoint
	// replaces them while requests are in flight. Everything that reads them
	// goes through catalog() and config().
	client llm.Catalog
	cfg    config.Config
}

func New(s *store.Store, l *agent.Loop, c llm.Catalog, cfg config.Config, ui fs.FS) *Server {
	return &Server{store: s, loop: l, client: c, cfg: cfg, ui: ui}
}

// config is the settings the running server is actually using, which is not
// always what the settings file says: a port change is saved and waits.
func (s *Server) config() config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *Server) catalog() llm.Catalog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// handler builds the routed, gated handler the server listens with. It is
// separate from Start so a test can drive the real routing table — the gate is
// part of the route, and testing a handler without it proves nothing about what
// the network can reach.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/auth", s.handleAuth)
	mux.HandleFunc("/api/auth/", s.handleAuthRoute)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/models", s.handleModels)
	mux.HandleFunc("/api/tasks", s.handleTasksList)
	mux.HandleFunc("/api/tasks/", s.handleTaskRoute)
	mux.HandleFunc("/api/breakdown", s.handleBreakdown)
	mux.HandleFunc("/api/groups/", s.handleGroupRoute)
	mux.HandleFunc(previewPrefix, s.handlePreview)
	mux.Handle("/", http.FileServer(http.FS(s.ui)))

	return s.withAuth(mux)
}

func (s *Server) Start(port int) error {
	addr := fmt.Sprintf(":%d", port)
	srv := &http.Server{
		Addr:        addr,
		Handler:     s.handler(),
		ReadTimeout: 30 * time.Second,
		// POST /api/breakdown is the one endpoint that blocks on model calls
		// rather than on the database — up to two of them, each bounded by the
		// client's 90s idle timeout. A write deadline shorter than that would
		// drop the connection while the request went on to build the group
		// anyway, leaving the caller with an error and a board with a new group
		// on it.
		WriteTimeout: breakdownBudget,
	}
	if s.authEnabled() {
		log.Println("API authentication is enabled (FANOUT_TOKEN)")
	} else {
		log.Println("API authentication is disabled - set FANOUT_TOKEN before exposing this server")
	}
	log.Printf("fanoutd server starting on %s\n", addr)

	s.mu.Lock()
	s.http = srv
	s.mu.Unlock()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown stops accepting requests and drains the ones in flight. It runs
// before the agent loops are drained so that nothing can POST /start into a
// server that is on its way out and orphan a fresh run.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv := s.http
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleConfig reports the settings that change what the board means rather
// than how it looks. An empty review column is two different facts — nothing is
// waiting, or nothing will ever be filed there — and the UI cannot tell them
// apart from the tasks alone. Gated like the rest of the API: it names the model
// the operator is paying for.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	cfg := s.config()
	json.NewEncoder(w).Encode(map[string]any{
		"review": cfg.Review,
		// Empty means the reviewer runs on whatever each task used, which is the
		// weak setting and is worth saying out loud where it is switched on.
		"review_model": cfg.ReviewModel,
		// From the loop rather than the config, so a sandbox that would not start
		// reads as off here too. A reviewer with a shell can run what it is
		// judging, which is the difference between checking work and reading it.
		"shell": s.loop.Sandboxed(),
	})
}

func (s *Server) handleTasksList(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listTasks(w, r)
	case http.MethodPost:
		s.createTask(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleTaskRoute(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 {
		http.Error(w, "invalid endpoint", http.StatusBadRequest)
		return
	}
	id := parts[2]

	if len(parts) == 4 {
		action := parts[3]
		switch action {
		case "move":
			s.moveTask(w, r, id)
		case "start":
			s.startTask(w, r, id)
		case "stop":
			s.stopTask(w, r, id)
		case "trace":
			s.getTrace(w, r, id)
		case "status":
			s.getTaskStatus(w, r, id)
		case "files":
			s.getFiles(w, r, id)
		case "raw":
			s.getRawFile(w, r, id)
		case "continue":
			s.continueTask(w, r, id)
		case "retry":
			s.retryTask(w, r, id)
		default:
			http.Error(w, "unknown action", http.StatusBadRequest)
		}
		return
	}

	if len(parts) > 3 {
		http.Error(w, "invalid endpoint", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getTask(w, r, id)
	case http.MethodPut:
		s.updateTask(w, r, id)
	case http.MethodDelete:
		s.deleteTask(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.store.ListTasks()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tasks)
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Goal        string `json:"goal"`
		Model       string `json:"model"`
		// Criteria is what review will hold the output to, one per line. A
		// breakdown writes these itself; a task made by hand gets them only if
		// whoever made it says what "done" means, and is judged on its goal alone
		// otherwise.
		Criteria string `json:"criteria"`
		// Seed is material to place in the new workspace before the task runs.
		Seed []models.SeedFile `json:"seed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	// Checked before the row is written, so a rejected seed leaves no task
	// behind that the client did not mean to create.
	if err := agent.ValidateSeed(req.Seed); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	task, err := s.store.CreateTaskFrom(store.NewTask{
		Title:       req.Title,
		Description: req.Description,
		Goal:        req.Goal,
		Criteria:    strings.TrimSpace(req.Criteria),
		Model:       req.Model,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.loop.SeedTask(task.ID, req.Seed); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(task)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request, id string) {
	task, err := s.store.GetTask(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if task == nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(task)
}

func (s *Server) updateTask(w http.ResponseWriter, r *http.Request, id string) {
	task, err := s.store.GetTask(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if task == nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Goal        string `json:"goal"`
		// Model is a pointer because "" is a real value here - it means fall
		// back to the configured default.
		Model *string `json:"model"`
		// Criteria is a pointer for the same reason: clearing them is a choice,
		// and it means the next review holds the work to the goal alone.
		Criteria *string `json:"criteria"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Title != "" {
		task.Title = req.Title
	}
	if req.Description != "" {
		task.Description = req.Description
	}
	if req.Goal != "" {
		task.Goal = req.Goal
	}
	if req.Model != nil {
		task.Model = strings.TrimSpace(*req.Model)
	}
	err = s.store.UpdateTask(task.ID, task.Title, task.Description, task.Goal, task.Column, task.Summary, task.Model, task.FinishFlag)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if req.Criteria != nil {
		if err := s.store.SetTaskCriteria(task.ID, strings.TrimSpace(*req.Criteria)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	updated, _ := s.store.GetTask(id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(updated)
}

// deleteTask removes a task, its trace, and - unless another task still shares
// it - its workspace directory. Passing ?files=keep leaves the output on disk.
func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request, id string) {
	task, err := s.store.GetTask(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if task == nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	s.loop.Stop(id)

	// Resolved before the row goes away, since the path comes from the task.
	ws, wsErr := s.loop.Workspace(id)

	if err := s.store.DeleteTask(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Build scratch goes unconditionally: files=keep is about the deliverables in
	// the workspace, and there are none in here.
	if err := s.loop.DiscardBuildDir(id); err != nil {
		log.Printf("deleted task %s but could not remove its build directory: %v\n", id, err)
	}

	if wsErr == nil && r.URL.Query().Get("files") != "keep" {
		remaining, err := s.store.CountTasksInWorkspace(workspaceID(task))
		if err != nil {
			log.Printf("deleted task %s but could not check its workspace: %v\n", id, err)
		} else if remaining == 0 {
			if err := os.RemoveAll(ws.Root()); err != nil {
				log.Printf("deleted task %s but could not remove %s: %v\n", id, ws.Root(), err)
			}
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

var validColumns = map[string]bool{
	models.ColumnIdeas:    true,
	models.ColumnTodo:     true,
	models.ColumnReview:   true,
	models.ColumnFinished: true,
}

const columnList = "ideas, todo, review, or finished"

func (s *Server) moveTask(w http.ResponseWriter, r *http.Request, id string) {
	task, err := s.store.GetTask(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if task == nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	var req struct {
		Column string `json:"column"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if !validColumns[req.Column] {
		http.Error(w, "invalid column. must be "+columnList, http.StatusBadRequest)
		return
	}

	// Moving a card only organizes it. The agent is started and stopped
	// explicitly; dragging a running task out of To-Do stops it.
	if req.Column != "todo" && s.loop.Stop(id) {
		log.Printf("stopped agent loop for task %s (moved to %s)\n", id, req.Column)
	}

	if err := s.store.SetTaskColumn(id, req.Column); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	updated, _ := s.store.GetTask(id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(updated)
}

func (s *Server) startTask(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.loop.Start(id); err != nil {
		if errors.Is(err, agent.ErrAlreadyRunning) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	task, _ := s.store.GetTask(id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"status": models.StatusRunning, "task": task})
}

func (s *Server) stopTask(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.loop.Stop(id) {
		task, err := s.store.GetTask(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if task == nil {
			http.Error(w, "task not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"status": task.Status, "task": task})
		return
	}

	// The loop writes its own final status; wait briefly so the response
	// reflects it rather than the stale "running".
	for i := 0; i < 20 && s.loop.IsRunning(id); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	task, _ := s.store.GetTask(id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"status": models.StatusStopped, "task": task})
}

func (s *Server) getFiles(w http.ResponseWriter, r *http.Request, id string) {
	files, err := s.loop.WorkspaceFiles(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(files)
}

// getRawFile serves one workspace file inline. Browsers refuse to follow a
// file:// link from an http:// page, so this is what actually opens a
// deliverable in a tab; the file:// path is offered alongside it for copying.
func (s *Server) getRawFile(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := r.URL.Query().Get("path")
	if rel == "" {
		http.Error(w, "path query parameter is required", http.StatusBadRequest)
		return
	}
	ws, err := s.loop.Workspace(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	full, err := ws.ResolvePath(rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(full)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	if info.IsDir() {
		http.Error(w, "path is a directory", http.StatusBadRequest)
		return
	}

	// Unknown types render as text rather than downloading; agent output is
	// text far more often than not.
	if mime.TypeByExtension(filepath.Ext(full)) == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("Content-Disposition", "inline; filename=\""+filepath.Base(full)+"\"")
	http.ServeFile(w, r, full)
}

// continueTask hands an existing workspace to a new task with a new goal. The
// original task, its trace, and its summary stay exactly as they were.
func (s *Server) continueTask(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parent, err := s.store.GetTask(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if parent == nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	var req struct {
		Title       string  `json:"title"`
		Description string  `json:"description"`
		Goal        string  `json:"goal"`
		Model       *string `json:"model"`
		Start       bool    `json:"start"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	goal := strings.TrimSpace(req.Goal)
	if goal == "" {
		http.Error(w, "goal is required", http.StatusBadRequest)
		return
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = nextTitle(parent.Title)
	}
	model := parent.Model
	if req.Model != nil {
		model = strings.TrimSpace(*req.Model)
	}

	task, err := s.store.CreateTaskFrom(store.NewTask{
		Title:       title,
		Description: strings.TrimSpace(req.Description),
		Goal:        goal,
		Model:       model,
		WorkspaceID: workspaceID(parent),
		ParentID:    parent.ID,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.respondNewTask(w, task, req.Start)
}

// retryTask re-runs the same brief from scratch: same title, description, goal,
// and model, but a clean workspace and no trace. The original run is untouched.
func (s *Server) retryTask(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	src, err := s.store.GetTask(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if src == nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	var req struct {
		Model *string `json:"model"`
		Start bool    `json:"start"`
	}
	// An empty body is a plain retry.
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&req)
	}
	model := src.Model
	if req.Model != nil {
		model = strings.TrimSpace(*req.Model)
	}

	task, err := s.store.CreateTaskFrom(store.NewTask{
		Title:       nextTitle(src.Title),
		Description: src.Description,
		Goal:        src.Goal,
		Model:       model,
		ParentID:    src.ID,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.respondNewTask(w, task, req.Start)
}

func (s *Server) respondNewTask(w http.ResponseWriter, task *models.Task, start bool) {
	if start {
		if err := s.loop.Start(task.ID); err != nil {
			log.Printf("created task %s but could not start it: %v\n", task.ID, err)
		} else if updated, err := s.store.GetTask(task.ID); err == nil && updated != nil {
			task = updated
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(task)
}

// nextTitle increments a "title (2)" suffix so a chain of continues and retries
// stays readable instead of stacking suffixes.
func nextTitle(title string) string {
	title = strings.TrimSpace(title)
	if strings.HasSuffix(title, ")") {
		if open := strings.LastIndex(title, " ("); open >= 0 {
			if n, err := strconv.Atoi(title[open+2 : len(title)-1]); err == nil && n > 0 {
				return fmt.Sprintf("%s (%d)", title[:open], n+1)
			}
		}
	}
	return title + " (2)"
}

func workspaceID(task *models.Task) string {
	if task.WorkspaceID != "" {
		return task.WorkspaceID
	}
	return task.ID
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	list, err := s.catalog().ListModels(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// handleBreakdown splits one idea into a group of subtasks. It blocks on the
// model, which is why it is the only endpoint with a budget of its own; the
// schedule it starts afterwards runs in the background as usual.
func (s *Server) handleBreakdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Idea string `json:"idea"`
		// Title names the fallback task, and is ignored when the split succeeds:
		// subtasks are titled by the breakdown.
		Title string `json:"title"`
		Model string `json:"model"`
		Start bool   `json:"start"`
		// Seed is material placed in the shared workspace before the subtasks
		// run, and shown to the planner so the split can account for it.
		Seed []models.SeedFile `json:"seed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Idea) == "" {
		http.Error(w, "idea is required", http.StatusBadRequest)
		return
	}
	// Rejected before the model call, which is the expensive half.
	if err := agent.ValidateSeed(req.Seed); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), breakdownBudget)
	defer cancel()

	result, err := s.loop.Breakdown(ctx, agent.BreakdownRequest{
		Title: strings.TrimSpace(req.Title),
		Idea:  strings.TrimSpace(req.Idea),
		Model: strings.TrimSpace(req.Model),
		Start: req.Start,
		Seed:  req.Seed,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if result.Fallback != "" {
		log.Printf("breakdown fell back to a single task: %s\n", result.Fallback)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(result)
}

func (s *Server) handleGroupRoute(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[2] == "" || len(parts) > 4 {
		http.Error(w, "invalid endpoint", http.StatusBadRequest)
		return
	}
	id := parts[2]

	// A bare group id is the group itself: GET is its plan, DELETE removes it.
	action := "plan"
	if len(parts) == 4 {
		action = parts[3]
	} else if r.Method == http.MethodDelete {
		action = "delete"
	}
	switch action {
	case "plan":
		s.getGroupPlan(w, r, id)
	case "start":
		s.startGroup(w, r, id)
	case "stop":
		s.stopGroup(w, r, id)
	case "move":
		s.moveGroup(w, r, id)
	case "delete":
		s.deleteGroup(w, r, id)
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}

// getGroupPlan serves the resolved waves and the state of every subtask, which
// is what both the board and `fanout plan --watch` render.
func (s *Server) getGroupPlan(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.writeGroupPlan(w, id)
}

func (s *Server) startGroup(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.loop.StartGroup(id); err != nil {
		switch {
		case errors.Is(err, agent.ErrGroupRunning):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, agent.ErrGroupNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	s.writeGroupPlan(w, id)
}

// stopGroup cancels a schedule and every subtask under it. Stopping a group that
// is not running is not an error - the caller wanted it stopped, and it is.
func (s *Server) stopGroup(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.loop.StopGroup(id) {
		// Subtasks record their own final status from their own goroutines, so
		// wait briefly for the plan to reflect the stop rather than the run.
		for i := 0; i < 20 && s.loop.IsGroupRunning(id); i++ {
			time.Sleep(50 * time.Millisecond)
		}
	}
	s.writeGroupPlan(w, id)
}

// moveGroup files every subtask of a breakdown at once. The board shows a group
// as one card, so the card has to move as one thing; per-task moves would leave
// a plan split across two columns with no way to say so.
//
// Moving anywhere but To-Do stops the schedule first, for the same reason
// dragging a single running task out of To-Do stops it: the columns are how a
// run is called off.
func (s *Server) moveGroup(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Column string `json:"column"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !validColumns[req.Column] {
		http.Error(w, "invalid column. must be "+columnList, http.StatusBadRequest)
		return
	}

	tasks, err := s.store.TasksInGroup(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(tasks) == 0 {
		http.Error(w, "group not found", http.StatusNotFound)
		return
	}

	if req.Column != "todo" {
		if s.loop.StopGroup(id) {
			log.Printf("stopped schedule for group %s (moved to %s)\n", id, req.Column)
		}
		for _, t := range tasks {
			s.loop.Stop(t.ID)
		}
	}

	for _, t := range tasks {
		if err := s.store.SetTaskColumn(t.ID, req.Column); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	s.writeGroupPlan(w, id)
}

// deleteGroup removes a whole breakdown: every subtask, every trace, and the one
// workspace they share. The workspace is deleted once at the end rather than per
// task, since the shared directory only becomes unreferenced after the last row
// goes. Passing ?files=keep leaves the output on disk.
func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tasks, err := s.store.TasksInGroup(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(tasks) == 0 {
		http.Error(w, "group not found", http.StatusNotFound)
		return
	}

	s.loop.StopGroup(id)

	// Resolved while the rows are still there, since the path comes from a task.
	ws, wsErr := s.loop.Workspace(tasks[0].ID)
	sharedID := workspaceID(&tasks[0])

	for _, t := range tasks {
		s.loop.Stop(t.ID)
		if err := s.store.DeleteTask(t.ID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Each subtask has build scratch of its own, however shared the workspace.
		if err := s.loop.DiscardBuildDir(t.ID); err != nil {
			log.Printf("deleted task %s but could not remove its build directory: %v\n", t.ID, err)
		}
	}

	if wsErr == nil && r.URL.Query().Get("files") != "keep" {
		remaining, err := s.store.CountTasksInWorkspace(sharedID)
		if err != nil {
			log.Printf("deleted group %s but could not check its workspace: %v\n", id, err)
		} else if remaining == 0 {
			if err := os.RemoveAll(ws.Root()); err != nil {
				log.Printf("deleted group %s but could not remove %s: %v\n", id, ws.Root(), err)
			}
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeGroupPlan(w http.ResponseWriter, id string) {
	plan, err := s.loop.GroupView(id)
	if err != nil {
		if errors.Is(err, agent.ErrGroupNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(plan)
}

func (s *Server) getTrace(w http.ResponseWriter, r *http.Request, id string) {
	steps, err := s.store.ListTraceSteps(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(steps)
}

func (s *Server) getTaskStatus(w http.ResponseWriter, r *http.Request, id string) {
	task, err := s.store.GetTask(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if task == nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  task.Status,
		"running": s.loop.IsRunning(id),
		"error":   task.Error,
		"task":    task,
	})
}
