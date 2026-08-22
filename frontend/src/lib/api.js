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

// fetchSettings returns every editable setting: what it is, what it is set to,
// and where that value came from. Secrets come back with an empty value and a
// `set` flag — the server never serves one, so the form can only replace them.
export async function fetchSettings() {
  return request('/settings');
}

// saveSettings sends only what changed. A secret left out of `values` is left
// alone; sending it empty clears it.
export async function saveSettings(values) {
  return request('/settings', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ values }),
  });
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

// breakdownStream splits one idea into a group of subtasks that share a
// workspace and run in dependency order, and reports the split as it happens:
// the server answers with one JSON object per line — phases while they change,
// snapshots of what the planner has written so far — and one final object
// carrying the result. onEvent sees every line except that last one; the
// resolved result is the return value. When the idea cannot be partitioned the
// server still resolves, with the reason in `fallback`.
export async function breakdownStream(body, onEvent) {
  const res = await fetch(`${API_BASE}/breakdown`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/x-ndjson' },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    // Errors before the stream opens are plain HTTP errors.
    const text = await res.text();
    const message = text.trim() || `${res.status} ${res.statusText}`;
    throw res.status === 401 ? new AuthError(message) : new Error(message);
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let pending = '';
  let result = null;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    pending += decoder.decode(value, { stream: true });
    let cut;
    while ((cut = pending.indexOf('\n')) >= 0) {
      const line = pending.slice(0, cut).trim();
      pending = pending.slice(cut + 1);
      if (!line) continue;
      let event;
      try {
        event = JSON.parse(line);
      } catch {
        continue; // a torn frame must not end the stream
      }
      if (event.kind === 'result') {
        result = event.result;
      } else if (event.kind === 'error') {
        throw new Error(event.message || 'the breakdown failed');
      } else if (onEvent) {
        onEvent(event);
      }
    }
  }
  if (!result) throw new Error('the breakdown ended without a result');
  return result;
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
