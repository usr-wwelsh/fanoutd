const API_BASE = '/api';

// AuthError is thrown on a 401 so callers can show the login prompt instead of
// treating an expired session as a generic failure.
export class AuthError extends Error {}

async function request(path, options = {}) {
  const res = await fetch(`${API_BASE}${path}`, options);
  if (!res.ok) {
    const body = await res.text();
    const message = body.trim() || `${res.status} ${res.statusText}`;
    throw res.status === 401 ? new AuthError(message) : new Error(message);
  }
  if (res.status === 204) return null;
  return res.json();
}

// fetchAuthStatus reports whether the server requires a token and whether this
// browser already holds a valid session cookie.
export async function fetchAuthStatus() {
  return request('/auth');
}

export async function login(token) {
  return request('/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ token }),
  });
}

export async function logout() {
  return request('/auth/logout', { method: 'POST' });
}

// fetchConfig reports the settings that change what the board means: whether a
// second agent reviews finished work, and what it runs on.
export async function fetchConfig() {
  return request('/config');
}

export async function fetchTasks() {
  return request('/tasks');
}

export async function createTask(task) {
  return request('/tasks', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(task),
  });
}

export async function updateTask(id, updates) {
  return request(`/tasks/${id}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(updates),
  });
}

// Deleting a task also removes its workspace directory unless another task
// shares it; pass keepFiles to leave the output on disk.
export async function deleteTask(id, keepFiles = false) {
  const query = keepFiles ? '?files=keep' : '';
  return request(`/tasks/${id}${query}`, { method: 'DELETE' });
}

export async function moveTask(id, column) {
  return request(`/tasks/${id}/move`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ column }),
  });
}

export async function startAgent(id) {
  return request(`/tasks/${id}/start`, { method: 'POST' });
}

export async function stopAgent(id) {
  return request(`/tasks/${id}/stop`, { method: 'POST' });
}

export async function fetchTrace(taskId) {
  return request(`/tasks/${taskId}/trace`);
}

export async function fetchFiles(taskId) {
  return request(`/tasks/${taskId}/files`);
}

export async function fetchTaskStatus(taskId) {
  return request(`/tasks/${taskId}/status`);
}

// continueTask starts a new task on the same workspace, leaving the original run intact.
export async function continueTask(taskId, body) {
  return request(`/tasks/${taskId}/continue`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
}

// retryTask re-runs the same brief in a clean workspace with no prior context.
export async function retryTask(taskId, body = {}) {
  return request(`/tasks/${taskId}/retry`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
}

// breakdown splits one idea into a group of subtasks that share a workspace and
// run in dependency order. It blocks on the model, so it is much slower than
// every other call here; when the idea cannot be partitioned the server returns
// a single ordinary task with the reason in `fallback`.
export async function breakdown(body) {
  return request('/breakdown', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
}

// fetchGroupPlan returns the resolved waves plus every subtask's current state.
export async function fetchGroupPlan(groupId) {
  return request(`/groups/${groupId}/plan`);
}

export async function startGroup(groupId) {
  return request(`/groups/${groupId}/start`, { method: 'POST' });
}

export async function stopGroup(groupId) {
  return request(`/groups/${groupId}/stop`, { method: 'POST' });
}

// A breakdown moves as one card, so every subtask is filed together. Moving
// anywhere but todo stops the schedule, exactly as it does for a single task.
export async function moveGroup(groupId, column) {
  return request(`/groups/${groupId}/move`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ column }),
  });
}

// deleteGroup removes every subtask and the one workspace they share.
export async function deleteGroup(groupId, keepFiles = false) {
  const query = keepFiles ? '?files=keep' : '';
  return request(`/groups/${groupId}${query}`, { method: 'DELETE' });
}

export async function fetchModels() {
  return request('/models');
}

// Browsers block file:// links opened from an http:// page, so this endpoint is
// what a click actually opens. fileUrl() is the copyable on-disk path.
export function rawFileUrl(taskId, path) {
  return `${API_BASE}/tasks/${taskId}/raw?path=${encodeURIComponent(path)}`;
}

// previewUrl serves a file from a workspace hosted as a site, so a page finds
// its own script and stylesheet by relative path. An empty path opens the
// workspace root, which is index.html if there is one.
export function previewUrl(taskId, path = '') {
  const rel = path.split('/').map(encodeURIComponent).join('/');
  return `/preview/${taskId}/${rel}`;
}

export function fileUrl(abs) {
  return `file://${abs.split('/').map(encodeURIComponent).join('/')}`;
}
