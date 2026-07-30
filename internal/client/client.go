// Package client talks to a fanoutd server over HTTP. It deliberately
// depends on nothing but net/http and internal/models, so a binary that links
// it cannot open a database by accident.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"fanoutd/internal/models"
)

// breakdownTimeout covers POST /api/breakdown, which blocks on one or two model
// calls while it partitions the idea. Every other endpoint answers out of the
// database, so they keep the shorter budget.
const breakdownTimeout = 5 * time.Minute

// Client is a handle on one server. The zero value is not usable; call New.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	// slow serves the one endpoint measured in minutes rather than seconds.
	slow *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		// Generous: a start call can wait on the first model response, and
		// nothing here streams.
		http: &http.Client{Timeout: 120 * time.Second},
		slow: &http.Client{Timeout: breakdownTimeout},
	}
}

func (c *Client) BaseURL() string { return c.baseURL }

// Error carries the server's status and message so callers can tell a missing
// task from an unreachable server.
type Error struct {
	Status int
	Body   string
	Path   string
}

func (e *Error) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s: %s", e.Path, http.StatusText(e.Status))
	}
	return fmt.Sprintf("%s: %s", e.Path, e.Body)
}

func (e *Error) NotFound() bool { return e.Status == http.StatusNotFound }

// Unauthorized reports the case worth its own message: the server wants a token
// and this client did not send a usable one.
func (e *Error) Unauthorized() bool { return e.Status == http.StatusUnauthorized }

// Conflict is a start call against a task that is already running.
func (e *Error) Conflict() bool { return e.Status == http.StatusConflict }

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.send(ctx, c.http, method, path, body, out)
}

func (c *Client) send(ctx context.Context, hc *http.Client, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, c.baseURL+path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// Server errors are plain text from http.Error, not JSON.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &Error{Status: resp.StatusCode, Body: strings.TrimSpace(string(msg)), Path: path}
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Health(ctx context.Context) error {
	var out map[string]string
	return c.do(ctx, http.MethodGet, "/api/health", nil, &out)
}

// AuthState reports whether the server requires a token and whether this
// client's token satisfies it.
type AuthState struct {
	Required      bool `json:"required"`
	Authenticated bool `json:"authenticated"`
}

func (c *Client) Auth(ctx context.Context) (*AuthState, error) {
	var out AuthState
	if err := c.do(ctx, http.MethodGet, "/api/auth", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListTasks(ctx context.Context) ([]models.Task, error) {
	var out []models.Task
	if err := c.do(ctx, http.MethodGet, "/api/tasks", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) GetTask(ctx context.Context, id string) (*models.Task, error) {
	var out models.Task
	if err := c.do(ctx, http.MethodGet, "/api/tasks/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// NewTask is the create body. Fields left empty are omitted so the server keeps
// its own defaults.
type NewTask struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Goal        string `json:"goal,omitempty"`
	Model       string `json:"model,omitempty"`
	// Seed is read from local paths by the caller and sent with the request, so
	// it reaches a board that has no access to this machine's disk.
	Seed []models.SeedFile `json:"seed,omitempty"`
}

func (c *Client) CreateTask(ctx context.Context, nt NewTask) (*models.Task, error) {
	var out models.Task
	if err := c.do(ctx, http.MethodPost, "/api/tasks", nt, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DeleteTask(ctx context.Context, id string, keepFiles bool) error {
	path := "/api/tasks/" + id
	if keepFiles {
		path += "?files=keep"
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

func (c *Client) MoveTask(ctx context.Context, id, column string) (*models.Task, error) {
	var out models.Task
	body := map[string]string{"column": column}
	if err := c.do(ctx, http.MethodPost, "/api/tasks/"+id+"/move", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RunState is the /start, /stop, and /status response shape. Running is only
// populated by /status; the other two imply it.
type RunState struct {
	Status  string       `json:"status"`
	Running bool         `json:"running"`
	Error   string       `json:"error"`
	Task    *models.Task `json:"task"`
}

func (c *Client) StartTask(ctx context.Context, id string) (*RunState, error) {
	return c.runAction(ctx, id, "start")
}

func (c *Client) StopTask(ctx context.Context, id string) (*RunState, error) {
	return c.runAction(ctx, id, "stop")
}

func (c *Client) runAction(ctx context.Context, id, action string) (*RunState, error) {
	var out RunState
	if err := c.do(ctx, http.MethodPost, "/api/tasks/"+id+"/"+action, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Status(ctx context.Context, id string) (*RunState, error) {
	var out RunState
	if err := c.do(ctx, http.MethodGet, "/api/tasks/"+id+"/status", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Trace(ctx context.Context, id string) ([]models.TraceStep, error) {
	var out []models.TraceStep
	if err := c.do(ctx, http.MethodGet, "/api/tasks/"+id+"/trace", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) Files(ctx context.Context, id string) ([]models.FileEntry, error) {
	var out []models.FileEntry
	if err := c.do(ctx, http.MethodGet, "/api/tasks/"+id+"/files", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Raw fetches one workspace file. It bypasses do because the response is the
// file itself rather than JSON.
func (c *Client) Raw(ctx context.Context, id, path string) ([]byte, error) {
	endpoint := fmt.Sprintf("/api/tasks/%s/raw?path=%s", id, url.QueryEscape(path))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+endpoint, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", c.baseURL+endpoint, err)
	}
	defer resp.Body.Close()

	data, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, &Error{Status: resp.StatusCode, Body: strings.TrimSpace(string(data)), Path: endpoint}
	}
	return data, readErr
}

// Followup is the body for both continue and retry. Continue requires a goal;
// retry ignores every field but Model and Start.
type Followup struct {
	Title       string  `json:"title,omitempty"`
	Description string  `json:"description,omitempty"`
	Goal        string  `json:"goal,omitempty"`
	Model       *string `json:"model,omitempty"`
	Start       bool    `json:"start,omitempty"`
}

func (c *Client) ContinueTask(ctx context.Context, id string, f Followup) (*models.Task, error) {
	return c.followup(ctx, id, "continue", f)
}

func (c *Client) RetryTask(ctx context.Context, id string, f Followup) (*models.Task, error) {
	return c.followup(ctx, id, "retry", f)
}

func (c *Client) followup(ctx context.Context, id, action string, f Followup) (*models.Task, error) {
	var out models.Task
	if err := c.do(ctx, http.MethodPost, "/api/tasks/"+id+"/"+action, f, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Idea is the breakdown body. Title names the fallback task and is ignored when
// the idea splits cleanly, since the subtasks are titled by the breakdown.
type Idea struct {
	Idea  string `json:"idea"`
	Title string `json:"title,omitempty"`
	Model string `json:"model,omitempty"`
	Start bool   `json:"start,omitempty"`
	// Seed lands in the shared workspace before the subtasks run, and is shown
	// to the planner so the split can be drawn around it.
	Seed []models.SeedFile `json:"seed,omitempty"`
}

// Breakdown splits an idea into a group of subtasks, or - when it cannot - into
// a single task with the reason in Fallback. It is the one call that waits on a
// model, so it gets its own timeout.
func (c *Client) Breakdown(ctx context.Context, idea Idea) (*models.BreakdownResult, error) {
	var out models.BreakdownResult
	if err := c.send(ctx, c.slow, http.MethodPost, "/api/breakdown", idea, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GroupPlan(ctx context.Context, groupID string) (*models.GroupPlan, error) {
	return c.group(ctx, http.MethodGet, groupID, "plan")
}

func (c *Client) StartGroup(ctx context.Context, groupID string) (*models.GroupPlan, error) {
	return c.group(ctx, http.MethodPost, groupID, "start")
}

func (c *Client) StopGroup(ctx context.Context, groupID string) (*models.GroupPlan, error) {
	return c.group(ctx, http.MethodPost, groupID, "stop")
}

func (c *Client) group(ctx context.Context, method, groupID, action string) (*models.GroupPlan, error) {
	var out models.GroupPlan
	if err := c.do(ctx, method, "/api/groups/"+groupID+"/"+action, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Models lists what the server will accept for --model, and which one it uses
// by default.
type Models struct {
	Default string `json:"default"`
	Models  []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"models"`
}

func (c *Client) ListModels(ctx context.Context) (*Models, error) {
	var out Models
	if err := c.do(ctx, http.MethodGet, "/api/models", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
