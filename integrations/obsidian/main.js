'use strict';

const { Notice, Plugin, PluginSettingTab, Setting } = require('obsidian');
const crypto = require('node:crypto');
const http = require('node:http');
const https = require('node:https');
const os = require('node:os');
const fs = require('node:fs');
const path = require('node:path');

const SCHEMA_VERSION = 1;
const MANAGED_START = '<!-- kovan:managed:v1 -->';
const MANAGED_END = '<!-- /kovan:managed -->';
const TASK_VIEW_START = '<!-- kovan:task:v1 -->';
const TASK_VIEW_END = '<!-- /kovan:task -->';
const MAX_RESPONSE_BYTES = 2 * 1024 * 1024;

const DEFAULT_SETTINGS = {
  mode: 'local',
  hostUrl: 'http://127.0.0.1:0',
  managedUrl: '',
  managedToken: '',
  projectName: 'Kovan',
  boardName: 'Inbox',
  repositoryId: '',
  targetDeviceId: 'local-mac',
  provider: 'codex',
  taskFolder: 'Kovan',
  templateRoot: '~/.agents/templates/kovan',
};

module.exports = class KovanLocalPlugin extends Plugin {
  async onload() {
    this.settings = Object.assign({}, DEFAULT_SETTINGS, await this.loadData());
    this.addSettingTab(new KovanSettingTab(this.app, this));
    this.addCommand({
      id: 'sync-active-task',
      name: 'Sync active Kovan task',
      callback: () => this.run('sync active task', () => this.syncActiveTask()),
    });
    this.addCommand({
      id: 'sync-all-managed-tasks',
      name: 'Sync all managed Kovan tasks',
      callback: () => this.run('sync managed tasks', () => this.syncAllManagedTasks()),
    });
    this.addCommand({
      id: 'update-task-from-note',
      name: 'Send active Kovan task edits to API',
      callback: () => this.run('update task', () => this.updateTaskFromActiveNote()),
    });
    this.addCommand({
      id: 'create-task-from-note',
      name: 'Create canonical Kovan task from active note',
      callback: () => this.run('create task', () => this.createTaskFromActiveNote()),
    });
    this.addCommand({
      id: 'import-templates',
      name: 'Import Kovan templates through the local API',
      callback: () => this.run('import templates', () => this.importTemplates()),
    });
  }

  async saveSettings() {
    await this.saveData(this.settings);
    this.client = null;
  }

  async run(label, operation) {
    try {
      await operation();
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      new Notice(`Kovan: ${label} failed — ${message}`);
    }
  }

  api() {
    if (String(this.settings.mode || 'local').toLowerCase() === 'managed') {
      const base = normalizeManagedURL(this.settings.managedUrl);
      if (!this.client || this.client.base !== base) {
        this.client = new ManagedCloudClient(base, this.settings.managedToken);
      }
      return this.client;
    }
    if (!this.client || this.client.base !== normalizeHostURL(this.settings.hostUrl)) {
      this.client = new LocalHostClient(this.settings.hostUrl);
    }
    return this.client;
  }

  activeMarkdownFile() {
    const file = this.app.workspace.getActiveFile();
    if (!file || file.extension !== 'md') {
      throw new Error('open a Markdown note first');
    }
    return file;
  }

  async syncActiveTask() {
    const file = this.activeMarkdownFile();
    const parsed = parseNote(await this.app.vault.read(file));
    if (!parsed.managed) {
      throw new Error('the active note has no canonical Kovan task ID');
    }
    await this.syncFile(file);
    new Notice(`Kovan: synced ${parsed.metadata.canonical_task_id}`);
  }

  async updateTaskFromActiveNote() {
    const file = this.activeMarkdownFile();
    const original = await this.app.vault.read(file);
    const parsed = parseNote(original);
    if (!parsed.managed || !parsed.taskBody) {
      throw new Error('the active note has no editable Kovan task view; sync or recreate it first');
    }
    const prompt = parsed.taskBody.trim();
    if (!prompt) throw new Error('the managed task view is empty');
    const title = noteTitle(file, prompt, parsed.metadata.title);
    const baseRevision = parsed.metadata.last_applied_revision;
    if (!(baseRevision > 0)) throw new Error('managed Kovan note has no base revision');
    const mutationKey = `obsidian:update:${stableKey(`${parsed.metadata.canonical_task_id}:${baseRevision}:${title}\n${prompt}`)}`;
    const client = this.api();
    const update = {
      task_id: parsed.metadata.canonical_task_id,
      revision: baseRevision,
      title,
      prompt,
      idempotency_key: mutationKey,
    };
    const response = typeof client.updateTask === 'function'
      ? await client.updateTask(parsed.metadata.canonical_task_id, update)
      : await client.patch(`/v1/tasks/${encodeURIComponent(parsed.metadata.canonical_task_id)}`, update, { 'If-Match': `"${baseRevision}"` });
    const task = response.task;
    const cursor = response.event?.cursor || parsed.metadata.last_applied_cursor || 0;
    const taskCursor = response.event?.task_cursor || parsed.metadata.last_applied_task_cursor || 0;
    const projected = applyTask(original, task, cursor, 'local', null, taskCursor);
    const current = await this.app.vault.read(file);
    if (current !== original) throw new Error(`${file.path} changed during update; refusing overwrite`);
    await this.app.vault.modify(file, projected);
    new Notice(`Kovan: updated ${task.id}`);
  }

  async syncAllManagedTasks() {
    const files = this.app.vault.getMarkdownFiles();
    let synced = 0;
    for (const file of files) {
      const parsed = parseNote(await this.app.vault.read(file));
      if (!parsed.managed) continue;
      await this.syncFile(file);
      synced += 1;
    }
    new Notice(`Kovan: synced ${synced} managed note${synced === 1 ? '' : 's'}`);
  }

  async syncFile(file) {
    const original = await this.app.vault.read(file);
    const parsed = parseNote(original);
    if (!parsed.managed) throw new Error(`${file.path} is not managed`);
    const taskID = parsed.metadata.canonical_task_id;
    const taskResponse = await this.api().get(`/v1/tasks/${encodeURIComponent(taskID)}`);
    const after = parsed.metadata.last_applied_cursor || 0;
    const legacyCursorState = after > 0 && !(parsed.metadata.last_applied_task_cursor > 0);
    const queryAfter = legacyCursorState ? 0 : after;
    let observed = await this.api().get(`/v1/tasks/${encodeURIComponent(taskID)}/events?after_cursor=${queryAfter}&limit=200`);
    let cursors;
    try {
      cursors = contiguousCursors(parsed.metadata.last_applied_cursor || 0, parsed.metadata.last_applied_task_cursor || 0, observed.events || []);
    } catch (error) {
      if (!(error instanceof CursorGap) || queryAfter === 0) throw error;
      observed = await this.api().get(`/v1/tasks/${encodeURIComponent(taskID)}/events?limit=200`);
      cursors = contiguousCursors(0, 0, observed.events || []);
    }
    const task = taskResponse.task || observed.task;
    const projected = applyTask(original, task, cursors.cursor, 'local', null, cursors.taskCursor);
    if (projected === original) return;
    const current = await this.app.vault.read(file);
    if (current !== original) throw new Error(`${file.path} changed during sync; refusing overwrite`);
    await this.app.vault.modify(file, projected);
  }

  async createTaskFromActiveNote() {
    const file = this.activeMarkdownFile();
    const original = await this.app.vault.read(file);
    const parsed = parseNote(original);
    if (parsed.managed) throw new Error('the active note already has a canonical task ID');
    if (this.api().requiresRepositoryID && !this.settings.repositoryId.trim()) throw new Error('configure a canonical repository ID first');
    const prompt = parsed.body.trim();
    if (!prompt) throw new Error('the active note is empty');
    const project = await this.api().post('/v1/projects', {
      name: this.settings.projectName.trim(),
      idempotency_key: `obsidian:project:${stableKey(this.settings.projectName)}`,
    });
    const board = await this.api().post('/v1/boards', {
      project_id: project.project.id,
      name: this.settings.boardName.trim(),
      idempotency_key: `obsidian:board:${stableKey(`${project.project.id}:${this.settings.boardName}`)}`,
    });
    const task = await this.api().post('/v1/tasks', {
      project_id: project.project.id,
      board_id: board.board.id,
      repository_id: this.settings.repositoryId.trim(),
      target_device_id: this.settings.targetDeviceId.trim() || 'local-mac',
      provider: this.settings.provider.trim() || 'codex',
      title: noteTitle(file, prompt),
      prompt,
      idempotency_key: `obsidian:task:${stableKey(file.path)}`,
    });
    const projected = applyTask('', task.task, 0, 'local', original.trim());
    const current = await this.app.vault.read(file);
    if (current !== original) throw new Error('the note changed while creating the canonical task');
    await this.app.vault.modify(file, projected);
    new Notice(`Kovan: created ${task.task.id}`);
  }

  async importTemplates() {
    if (this.api().requiresRepositoryID && !this.settings.repositoryId.trim()) throw new Error('configure a canonical repository ID first');
    const root = expandHome(this.settings.templateRoot);
    const projectFile = path.join(root, 'project.example.yaml');
    const taskFile = path.join(root, 'task.md');
    const projectText = fs.readFileSync(projectFile, 'utf8');
    const taskText = fs.readFileSync(taskFile, 'utf8');
    const projectName = yamlScalar(projectText, 'name') || this.settings.projectName;
    const title = frontmatterScalar(taskText, 'title') || 'Imported Kovan task';
    const prompt = markdownPrompt(taskText);
    if (!prompt) throw new Error('the task template has no prompt body');
    const project = await this.api().post('/v1/projects', {
      name: projectName,
      idempotency_key: 'obsidian:template:project:v1',
    });
    const board = await this.api().post('/v1/boards', {
      project_id: project.project.id,
      name: this.settings.boardName.trim(),
      idempotency_key: 'obsidian:template:board:v1',
    });
    const response = await this.api().post('/v1/tasks', {
      project_id: project.project.id,
      board_id: board.board.id,
      repository_id: this.settings.repositoryId.trim(),
      target_device_id: this.settings.targetDeviceId.trim() || 'local-mac',
      provider: this.settings.provider.trim() || 'codex',
      title,
      prompt,
      idempotency_key: 'obsidian:template:task:v1',
    });
    const folder = this.settings.taskFolder || 'Kovan';
    await ensureFolder(this.app.vault, folder);
    const filename = `${folder}/${safeFileName(title)}.md`;
    const content = replaceManaged('', { managed: false, body: '' }, {
      schema_version: SCHEMA_VERSION,
      canonical_task_id: response.task.id,
      project_id: response.task.project_id,
      board_id: response.task.board_id,
      title: response.task.title,
      state: response.task.state,
      last_applied_revision: response.task.revision,
      last_applied_cursor: 0,
      last_applied_task_cursor: 0,
      sync_state: 'imported',
      source_imported: true,
      template_id: 'kovan.task/v1',
    }, prompt);
    const existing = this.app.vault.getAbstractFileByPath(filename);
    if (!existing) await this.app.vault.create(filename, content);
    new Notice(`Kovan: imported template as ${response.task.id}`);
  }
};

class KovanSettingTab extends PluginSettingTab {
  constructor(app, plugin) {
    super(app, plugin);
    this.plugin = plugin;
  }

  display() {
    const { containerEl } = this;
    containerEl.empty();
    containerEl.createEl('h2', { text: 'Kovan Local' });
    textSetting(containerEl, 'Mode', 'Use local for Desktop/AgentBridge or managed for the authenticated Kovan Control API.', this.plugin, 'mode');
    textSetting(containerEl, 'Desktop host URL', 'The loopback URL printed by desktop/host.js.', this.plugin, 'hostUrl');
    textSetting(containerEl, 'Managed Control URL', 'HTTPS URL for the private Kovan Control API; loopback HTTP is allowed only for local tests.', this.plugin, 'managedUrl');
    secretSetting(containerEl, 'Managed bearer token', 'Used only for managed API authentication; never written into notes.', this.plugin, 'managedToken');
    textSetting(containerEl, 'Project name', 'Used for API-first note creation.', this.plugin, 'projectName');
    textSetting(containerEl, 'Board name', 'Used for API-first note creation.', this.plugin, 'boardName');
    textSetting(containerEl, 'Repository ID', 'Opaque canonical ID; never a filesystem path.', this.plugin, 'repositoryId');
    textSetting(containerEl, 'Target device ID', 'Usually local-mac or a paired device ID.', this.plugin, 'targetDeviceId');
    textSetting(containerEl, 'Provider', 'Provider selected through the AgentBridge registry.', this.plugin, 'provider');
    textSetting(containerEl, 'Task folder', 'Vault-relative folder for imported canonical tasks.', this.plugin, 'taskFolder');
    textSetting(containerEl, 'Template root', 'Local template source imported once through the API.', this.plugin, 'templateRoot');
    new Setting(containerEl)
      .setName('Save settings')
      .setDesc('Local mode keeps only the loopback proxy URL and opaque IDs; the local API secret is never stored here. Managed mode stores its bearer token for Control API requests.')
      .addButton((button) => button.setButtonText('Save').setCta().onClick(() => this.plugin.saveSettings()));
  }
}

function textSetting(container, name, description, plugin, key) {
  new Setting(container)
    .setName(name)
    .setDesc(description)
    .addText((text) => text.setValue(plugin.settings[key] || '').onChange((value) => {
      plugin.settings[key] = value;
    }));
}

function secretSetting(container, name, description, plugin, key) {
  new Setting(container)
    .setName(name)
    .setDesc(description)
    .addText((text) => {
      text.setValue(plugin.settings[key] || '').onChange((value) => {
        plugin.settings[key] = value;
      });
      if (text.inputEl) text.inputEl.type = 'password';
    });
}

class CursorGap extends Error {}

class LocalHostClient {
  constructor(base) {
    this.base = normalizeHostURL(base);
    this.cookie = '';
    this.requiresRepositoryID = true;
  }

  async get(endpoint) {
    return this.request('GET', endpoint);
  }

  async post(endpoint, body) {
    return this.request('POST', endpoint, body);
  }

  async patch(endpoint, body, headers = {}) {
    return this.request('PATCH', endpoint, body, headers);
  }

  async request(method, endpoint, body, headers = {}, retry = true) {
    await this.ensureSession();
    const response = await requestRaw(this.base + endpoint, {
      method,
      cookie: this.cookie,
      body,
      headers,
    });
    if (response.status === 401 && retry) {
      this.cookie = '';
      return this.request(method, endpoint, body, headers, false);
    }
    if (response.status < 200 || response.status >= 300) {
      let code = `HTTP ${response.status}`;
      try {
        const parsed = JSON.parse(response.body);
        if (parsed.error) code = parsed.error;
      } catch (_) {
        // Preserve the stable HTTP error when the proxy did not return JSON.
      }
      throw new Error(code);
    }
    if (!response.body) return {};
    return JSON.parse(response.body);
  }

  async ensureSession() {
    if (this.cookie) return;
    const response = await requestRaw(this.base + '/', { method: 'GET' });
    if (response.status !== 200) throw new Error(`Desktop host unavailable (${response.status})`);
    this.cookie = response.cookie;
    if (!this.cookie) throw new Error('Desktop host did not issue a session');
  }
}

class ManagedCloudClient {
  constructor(base, token) {
    this.base = normalizeManagedURL(base);
    this.token = String(token || '').trim();
    this.requiresRepositoryID = false;
  }

  async get(endpoint) {
    const response = await this.request('GET', endpoint);
    return normalizeManagedResponse(endpoint, response);
  }

  async post(endpoint, body) {
    let path = endpoint;
    let input = body || {};
    if (endpoint === '/v1/boards') {
      const projectID = String(input.project_id || '').trim();
      if (!projectID) throw new Error('managed project ID is required');
      path = `/v1/projects/${encodeURIComponent(projectID)}/boards`;
      input = { name: input.name, columns: input.columns || [] };
    } else if (endpoint === '/v1/projects') {
      input = { name: input.name };
    } else if (endpoint === '/v1/tasks') {
      input = { project_id: input.project_id, board_id: input.board_id, title: input.title, prompt: input.prompt };
    }
    const response = await this.request('POST', path, input);
    return normalizeManagedResponse(endpoint, response);
  }

  async updateTask(taskID, body) {
    const current = await this.get(`/v1/tasks/${encodeURIComponent(taskID)}`);
    const currentTask = current.task;
    const response = await this.request('PATCH', `/v1/tasks/${encodeURIComponent(taskID)}`, {
      revision: body.revision,
      title: body.title,
      prompt: body.prompt,
      status: currentTask.status || currentTask.state || 'backlog',
    }, { 'If-Match': `"${body.revision}"` });
    return { task: normalizeTask(response.task || response), ...(response.event ? { event: normalizeEvent(response.event) } : {}) };
  }

  async request(method, endpoint, body, headers = {}) {
    if (!this.token) throw new Error('managed bearer token is not configured');
    const response = await requestManagedRaw(this.base + endpoint, {
      method,
      token: this.token,
      body,
      headers,
    });
    if (response.status < 200 || response.status >= 300) {
      let code = `HTTP ${response.status}`;
      try {
        const parsed = JSON.parse(response.body);
        if (parsed.code || parsed.error) code = parsed.code || parsed.error;
      } catch (_) {
        // Preserve the stable HTTP error when the control API did not return JSON.
      }
      throw new Error(code);
    }
    if (!response.body) return {};
    return JSON.parse(response.body);
  }
}

function requestRaw(rawURL, options) {
  const parsed = new URL(rawURL);
  if (!['http:', 'https:'].includes(parsed.protocol) || parsed.username || parsed.password || !isLoopback(parsed.hostname)) {
    return Promise.reject(new Error('Desktop host URL must be an authenticated loopback URL'));
  }
  const transport = parsed.protocol === 'https:' ? https : http;
  const headers = { Accept: 'application/json' };
  Object.assign(headers, options.headers || {});
  if (options.cookie) headers.Cookie = options.cookie;
  let payload = null;
  if (options.body !== undefined) {
    payload = Buffer.from(JSON.stringify(options.body));
    headers['Content-Type'] = 'application/json';
    headers['Content-Length'] = payload.length;
  }
  return new Promise((resolve, reject) => {
    const request = transport.request(parsed, { method: options.method, headers }, (response) => {
      const chunks = [];
      let size = 0;
      response.on('data', (chunk) => {
        size += chunk.length;
        if (size <= MAX_RESPONSE_BYTES) chunks.push(chunk);
      });
      response.on('end', () => {
        if (size > MAX_RESPONSE_BYTES) {
          reject(new Error('Desktop host response is too large'));
          return;
        }
        const cookies = response.headers['set-cookie'] || [];
        const setCookie = Array.isArray(cookies) ? cookies[0] : cookies;
        resolve({
          status: response.statusCode || 0,
          body: Buffer.concat(chunks).toString('utf8'),
          cookie: String(setCookie || '').split(';', 1)[0],
        });
      });
    });
    request.setTimeout(30000, () => request.destroy(new Error('Desktop host request timed out')));
    request.on('error', reject);
    if (payload) request.write(payload);
    request.end();
  });
}

function requestManagedRaw(rawURL, options) {
  const parsed = new URL(rawURL);
  if (parsed.username || parsed.password || (parsed.protocol !== 'https:' && !(parsed.protocol === 'http:' && isLoopback(parsed.hostname)))) {
    return Promise.reject(new Error('managed Control API URL must use HTTPS'));
  }
  const transport = parsed.protocol === 'https:' ? https : http;
  const headers = { Accept: 'application/json', Authorization: `Bearer ${options.token}` };
  Object.assign(headers, options.headers || {});
  let payload = null;
  if (options.body !== undefined) {
    payload = Buffer.from(JSON.stringify(options.body));
    headers['Content-Type'] = 'application/json';
    headers['Content-Length'] = payload.length;
  }
  return new Promise((resolve, reject) => {
    const request = transport.request(parsed, { method: options.method, headers }, (response) => {
      const chunks = [];
      let size = 0;
      response.on('data', (chunk) => {
        size += chunk.length;
        if (size <= MAX_RESPONSE_BYTES) chunks.push(chunk);
      });
      response.on('end', () => {
        if (size > MAX_RESPONSE_BYTES) {
          reject(new Error('Control API response is too large'));
          return;
        }
        resolve({ status: response.statusCode || 0, body: Buffer.concat(chunks).toString('utf8') });
      });
    });
    request.setTimeout(30000, () => request.destroy(new Error('Control API request timed out')));
    request.on('error', reject);
    if (payload) request.write(payload);
    request.end();
  });
}

function normalizeHostURL(value) {
  const parsed = new URL(String(value || ''));
  if (!['http:', 'https:'].includes(parsed.protocol) || parsed.username || parsed.password || !isLoopback(parsed.hostname)) {
    throw new Error('Desktop host URL must be an authenticated loopback URL');
  }
  parsed.pathname = parsed.pathname.replace(/\/+$/, '');
  parsed.search = '';
  parsed.hash = '';
  return parsed.toString().replace(/\/$/, '');
}

function normalizeManagedURL(value) {
  const parsed = new URL(String(value || ''));
  if (parsed.username || parsed.password || (parsed.protocol !== 'https:' && !(parsed.protocol === 'http:' && isLoopback(parsed.hostname)))) {
    throw new Error('managed Control API URL must use HTTPS');
  }
  parsed.pathname = parsed.pathname.replace(/\/+$/, '');
  parsed.search = '';
  parsed.hash = '';
  return parsed.toString().replace(/\/$/, '');
}

function isLoopback(hostname) {
  return hostname === 'localhost' || hostname === '127.0.0.1' || hostname === '::1' || hostname === '[::1]';
}

function parseNote(content) {
  const start = content.indexOf(MANAGED_START);
  if (start < 0) return { managed: false, metadata: null, body: content, taskBody: '', hasTaskView: false, start: -1, end: -1, taskStart: -1, taskEnd: -1 };
  const markerStart = start + MANAGED_START.length;
  const endOffset = content.indexOf(MANAGED_END, markerStart);
  if (endOffset < 0) throw new Error('managed Kovan marker is incomplete');
  let metadata;
  try {
    metadata = JSON.parse(content.slice(markerStart, endOffset).trim());
  } catch (_) {
    throw new Error('managed Kovan metadata is invalid JSON');
  }
  if (metadata.schema_version !== SCHEMA_VERSION || !metadata.canonical_task_id) throw new Error('managed Kovan metadata is invalid');
  const end = endOffset + MANAGED_END.length;
  let body = content.slice(0, start) + content.slice(end);
  const taskStart = body.indexOf(TASK_VIEW_START);
  if (taskStart < 0) return { managed: true, metadata, body, taskBody: '', hasTaskView: false, start, end, taskStart: -1, taskEnd: -1 };
  const taskOffset = taskStart + TASK_VIEW_START.length;
  const taskEndOffset = body.indexOf(TASK_VIEW_END, taskOffset);
  if (taskEndOffset < 0) throw new Error('managed Kovan task view is incomplete');
  const taskEnd = taskEndOffset + TASK_VIEW_END.length;
  const taskBody = body.slice(taskOffset, taskEndOffset);
  body = body.slice(0, taskStart) + body.slice(taskEnd);
  return { managed: true, metadata, body, taskBody, hasTaskView: true, start, end, taskStart, taskEnd };
}

function normalizeTask(value) {
  const task = value && value.task ? value.task : value;
  if (!task || typeof task !== 'object') return task;
  const state = task.state || task.status || '';
  return { ...task, state, status: task.status || state };
}

function normalizeEvent(value) {
  if (!value || typeof value !== 'object') return value;
  return { ...value, task_cursor: Number(value.task_cursor || value.taskCursor || 0) };
}

function normalizeManagedResponse(endpoint, value) {
  const pathname = endpoint.split('?', 1)[0];
  if (pathname.endsWith('/events')) {
    const events = Array.isArray(value) ? value : (value.events || []);
    return { ...(Array.isArray(value) ? {} : value), events: events.map(normalizeEvent) };
  }
  if (/^\/v1\/tasks\/[^/]+$/.test(pathname)) return { ...(value && value.task ? value : {}), task: normalizeTask(value) };
  if (pathname === '/v1/projects') return { ...(value && value.project ? value : {}), project: value.project ? value.project : value };
  if (pathname === '/v1/boards' || /\/boards$/.test(pathname)) return { ...(value && value.board ? value : {}), board: value.board ? value.board : value };
  if (pathname === '/v1/tasks') return { ...(value && value.task ? value : {}), task: normalizeTask(value) };
  return value;
}

function contiguousCursors(currentGlobal, currentTask, events) {
  const values = [...events];
  const hasTaskCursors = values.length > 0 && values.every((event) => Number(event.task_cursor) > 0);
  if (hasTaskCursors) {
    values.sort((left, right) => left.task_cursor - right.task_cursor);
    let taskCursor = currentTask || 0;
    let cursor = currentGlobal || 0;
    if (taskCursor === 0 && cursor > 0) {
      if (values[0].task_cursor !== 1) throw new CursorGap();
    }
    for (const event of values) {
      if (event.task_cursor <= taskCursor) continue;
      if (event.task_cursor !== taskCursor + 1) throw new CursorGap();
      taskCursor = event.task_cursor;
      cursor = Math.max(cursor, event.cursor || 0);
    }
    return { cursor, taskCursor };
  }
  let cursor = currentGlobal || 0;
  for (const event of values.sort((left, right) => left.cursor - right.cursor)) {
    if (event.cursor <= cursor) continue;
    if (event.cursor !== cursor + 1) throw new CursorGap();
    cursor = event.cursor;
  }
  return { cursor, taskCursor: currentTask || 0 };
}

function applyTask(original, task, cursor, state, taskBodyOverride = null, taskCursor = cursor) {
  if (!task || !task.id || !task.project_id || !task.board_id || !(task.revision > 0)) throw new Error('local API returned an invalid task');
  const parsed = parseNote(original);
  if (parsed.managed && parsed.metadata.canonical_task_id !== task.id) throw new Error('note and API task IDs differ');
  if (parsed.managed && (task.revision < parsed.metadata.last_applied_revision ||
    (task.revision === parsed.metadata.last_applied_revision && cursor < parsed.metadata.last_applied_cursor) ||
    (task.revision === parsed.metadata.last_applied_revision && taskCursor < (parsed.metadata.last_applied_task_cursor || 0)))) {
    throw new Error('canonical task revision is stale');
  }
  const taskBody = taskBodyOverride === null ? task.prompt || parsed.taskBody : taskBodyOverride;
  return replaceManaged(original, parsed, {
    schema_version: SCHEMA_VERSION,
    canonical_task_id: task.id,
    project_id: task.project_id,
    board_id: task.board_id,
    title: task.title,
    state: task.state,
    last_applied_revision: task.revision,
    last_applied_cursor: cursor,
    last_applied_task_cursor: taskCursor,
    sync_state: state,
    ...(parsed.managed && parsed.metadata.source_imported ? { source_imported: true } : {}),
    ...(parsed.managed && parsed.metadata.template_id ? { template_id: parsed.metadata.template_id } : {}),
  }, taskBody);
}

function replaceManaged(original, parsed, metadata, taskBody) {
  const metadataBlock = `${MANAGED_START}\n${JSON.stringify(metadata, null, 2)}\n${MANAGED_END}`;
  const taskBlock = `${TASK_VIEW_START}\n${String(taskBody || '').trim()}\n${TASK_VIEW_END}`;
  const block = `${metadataBlock}\n\n${taskBlock}`;
  if (!parsed.managed) return original.trim() ? `${block}\n\n${original}` : `${block}\n`;
  const personal = parsed.body.replace(/^\n+/, '');
  return personal ? `${block}\n\n${personal}` : `${block}\n`;
}

function stableKey(value) {
  return crypto.createHash('sha256').update(String(value)).digest('hex');
}

function noteTitle(file, prompt, fallback = '') {
  const heading = prompt.match(/^\s*#\s+(.+)$/m);
  return heading ? heading[1].trim() : fallback || file.basename;
}

function expandHome(value) {
  const raw = String(value || '').trim();
  return raw.startsWith('~/') ? path.join(os.homedir(), raw.slice(2)) : raw;
}

function yamlScalar(contents, key) {
  const match = contents.match(new RegExp(`^${key}:\\s*(.+)$`, 'm'));
  return match ? match[1].trim().replace(/^['"]|['"]$/g, '') : '';
}

function frontmatterScalar(contents, key) {
  const section = contents.match(/^---\n([\s\S]*?)\n---\n?/);
  return section ? yamlScalar(section[1], key) : '';
}

function markdownPrompt(contents) {
  const body = contents.replace(/^---\n[\s\S]*?\n---\n?/, '').trim();
  return body;
}

function safeFileName(value) {
  const name = String(value || 'Kovan task').replace(/[^A-Za-z0-9._ -]+/g, '').trim().replace(/\s+/g, '-');
  return (name || 'kovan-task').slice(0, 96);
}

async function ensureFolder(vault, folder) {
  const parts = folder.split('/').filter(Boolean);
  let current = '';
  for (const part of parts) {
    current = current ? `${current}/${part}` : part;
    if (!vault.getAbstractFileByPath(current)) await vault.createFolder(current);
  }
}

module.exports.ManagedCloudClient = ManagedCloudClient;
module.exports.normalizeManagedResponse = normalizeManagedResponse;
