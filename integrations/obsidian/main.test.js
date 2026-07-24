const assert = require('node:assert/strict');
const Module = require('node:module');
const http = require('node:http');
const { once } = require('node:events');
const test = require('node:test');

class StubPlugin {
  async loadData() {
    return { hostUrl: 'http://127.0.0.1:0' };
  }

  async saveData() {}

  addCommand(command) {
    this.commands = this.commands || [];
    this.commands.push(command);
  }

  addSettingTab() {}
}

class StubPluginSettingTab {
  constructor(app, plugin) {
    this.app = app;
    this.plugin = plugin;
  }
}

class StubSetting {
  setName() { return this; }
  setDesc() { return this; }
  addButton() { return this; }
  addText() { return this; }
  setValue() { return this; }
}

function loadPlugin() {
  const originalLoad = Module._load;
  Module._load = function load(request, parent, isMain) {
    if (request === 'obsidian') {
      return { Notice: class {}, Plugin: StubPlugin, PluginSettingTab: StubPluginSettingTab, Setting: StubSetting };
    }
    return originalLoad.call(this, request, parent, isMain);
  };
  try {
    delete require.cache[require.resolve('./main.js')];
    return require('./main.js');
  } finally {
    Module._load = originalLoad;
  }
}

test('Obsidian task edits send only the managed task view with its base revision', async () => {
  const Plugin = loadPlugin();
  const original = `<!-- kovan:managed:v1 -->
{
  "schema_version": 1,
  "canonical_task_id": "task-1",
  "project_id": "project-1",
  "board_id": "board-1",
  "title": "Old title",
  "state": "queued",
  "last_applied_revision": 3,
  "last_applied_cursor": 7,
  "sync_state": "local"
}
<!-- /kovan:managed -->

<!-- kovan:task:v1 -->
# New title

Edited prompt only.
<!-- /kovan:task -->

Personal notes must stay local.
`;
  const file = { extension: 'md', path: 'Tasks/task-1.md', basename: 'task-1' };
  const modified = [];
  const app = {
    workspace: { getActiveFile: () => file },
    vault: {
      read: async () => original,
      modify: async (_file, content) => modified.push(content),
    },
  };
  const plugin = new Plugin();
  plugin.app = app;
  await plugin.onload();
  let request;
  plugin.client = {
    base: 'http://127.0.0.1:0',
    patch: async (endpoint, body, headers) => {
      request = { endpoint, body, headers };
      return {
        task: {
          id: 'task-1', project_id: 'project-1', board_id: 'board-1', title: 'New title',
          prompt: '# New title\n\nEdited prompt only.', state: 'queued', revision: 4,
        },
        event: { cursor: 9 },
      };
    },
  };

  await plugin.updateTaskFromActiveNote();

  assert.equal(request.endpoint, '/v1/tasks/task-1');
  assert.equal(request.body.task_id, 'task-1');
  assert.equal(request.body.revision, 3);
  assert.equal(request.headers['If-Match'], '"3"');
  assert.equal(request.body.title, 'New title');
  assert.equal(request.body.prompt, '# New title\n\nEdited prompt only.');
  assert.match(request.body.idempotency_key, /^obsidian:update:[0-9a-f]{64}$/);
  assert.equal(modified.length, 1);
  assert.match(modified[0], /"last_applied_revision": 4/);
  assert.match(modified[0], /"last_applied_cursor": 9/);
  assert.match(modified[0], /# New title/);
  assert.match(modified[0], /Personal notes must stay local\./);
  assert.doesNotMatch(request.body.prompt, /Personal notes/);
});

test('Obsidian sync uses task cursors across interleaved global events', async () => {
  const Plugin = loadPlugin();
  const original = `<!-- kovan:managed:v1 -->
{
  "schema_version": 1,
  "canonical_task_id": "task-1",
  "project_id": "project-1",
  "board_id": "board-1",
  "title": "Task",
  "state": "running",
  "last_applied_revision": 1,
  "last_applied_cursor": 1,
  "last_applied_task_cursor": 1,
  "sync_state": "local"
}
<!-- /kovan:managed -->

<!-- kovan:task:v1 -->
Run task.
<!-- /kovan:task -->

Personal notes stay local.
`;
  const file = { extension: 'md', path: 'Tasks/task-1.md', basename: 'task-1' };
  const modified = [];
  const plugin = new Plugin();
  plugin.app = {
    vault: {
      read: async () => original,
      modify: async (_file, content) => modified.push(content),
    },
  };
  plugin.settings = { hostUrl: 'http://127.0.0.1:0' };
  plugin.client = {
    base: 'http://127.0.0.1:0',
    get: async (endpoint) => {
      if (endpoint === '/v1/tasks/task-1') {
        return { task: { id: 'task-1', project_id: 'project-1', board_id: 'board-1', title: 'Task', prompt: 'Run task.', state: 'running', revision: 3 } };
      }
      assert.match(endpoint, /after_cursor=1/);
      return {
        events: [
          { cursor: 3, task_cursor: 2, id: 'event-2' },
          { cursor: 5, task_cursor: 3, id: 'event-3' },
        ],
      };
    },
  };

  await plugin.syncFile(file);

  assert.equal(modified.length, 1);
  assert.match(modified[0], /"last_applied_cursor": 5/);
  assert.match(modified[0], /"last_applied_task_cursor": 3/);
  assert.match(modified[0], /Personal notes stay local\./);
});

test('managed Obsidian client normalizes Control API responses and strips local-only fields', async () => {
  const Plugin = loadPlugin();
  const ManagedCloudClient = Plugin.ManagedCloudClient;
  const seen = [];
  const server = http.createServer((request, response) => {
    const chunks = [];
    request.on('data', (chunk) => chunks.push(chunk));
    request.on('end', () => {
      const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString('utf8')) : null;
      seen.push({ method: request.method, url: request.url, authorization: request.headers.authorization, ifMatch: request.headers['if-match'], body });
      response.setHeader('Content-Type', 'application/json');
      if (request.method === 'POST' && request.url === '/v1/projects') {
        response.end(JSON.stringify({ id: 'project-1', name: 'Kovan', revision: 1 }));
        return;
      }
      if (request.method === 'POST' && request.url === '/v1/projects/project-1/boards') {
        response.end(JSON.stringify({ id: 'board-1', project_id: 'project-1', name: 'Inbox', revision: 1 }));
        return;
      }
      if (request.method === 'POST' && request.url === '/v1/tasks') {
        response.end(JSON.stringify({ id: 'task-1', project_id: 'project-1', board_id: 'board-1', title: 'Task', prompt: 'Run', status: 'backlog', revision: 1 }));
        return;
      }
      if (request.method === 'GET' && request.url === '/v1/tasks/task-1') {
        response.end(JSON.stringify({ id: 'task-1', project_id: 'project-1', board_id: 'board-1', title: 'Task', prompt: 'Run', status: 'in_progress', revision: 2 }));
        return;
      }
      if (request.method === 'GET' && request.url.startsWith('/v1/tasks/task-1/events')) {
        response.end(JSON.stringify([{ id: 'event-1', cursor: 4, task_cursor: 2, revision: 2, type: 'TaskUpdated' }]));
        return;
      }
      if (request.method === 'PATCH' && request.url === '/v1/tasks/task-1') {
        response.end(JSON.stringify({ id: 'task-1', project_id: 'project-1', board_id: 'board-1', title: body.title, prompt: body.prompt, status: body.status, revision: 3 }));
        return;
      }
      response.writeHead(404);
      response.end(JSON.stringify({ code: 'not_found' }));
    });
  });
  await once(server.listen(0, '127.0.0.1'), 'listening');
  const address = server.address();
  const client = new ManagedCloudClient(`http://127.0.0.1:${address.port}`, 'managed-token');
  try {
    const project = await client.post('/v1/projects', { name: 'Kovan', idempotency_key: 'local-only' });
    assert.equal(project.project.id, 'project-1');
    const board = await client.post('/v1/boards', { project_id: 'project-1', name: 'Inbox', idempotency_key: 'local-only' });
    assert.equal(board.board.id, 'board-1');
    const task = await client.post('/v1/tasks', { project_id: 'project-1', board_id: 'board-1', repository_id: '/never-a-path', target_device_id: 'local-mac', provider: 'codex', title: 'Task', prompt: 'Run', idempotency_key: 'local-only' });
    assert.equal(task.task.state, 'backlog');
    const observed = await client.get('/v1/tasks/task-1/events?after_cursor=2&limit=200');
    assert.equal(observed.events[0].task_cursor, 2);
    const updated = await client.updateTask('task-1', { revision: 2, title: 'Updated', prompt: 'Run now', idempotency_key: 'local-only' });
    assert.equal(updated.task.state, 'in_progress');
    assert.equal(seen.every((request) => request.authorization === 'Bearer managed-token'), true);
    const boardRequest = seen.find((request) => request.url === '/v1/projects/project-1/boards');
    assert.deepEqual(boardRequest.body, { name: 'Inbox', columns: [] });
    const taskRequest = seen.find((request) => request.method === 'POST' && request.url === '/v1/tasks');
    assert.deepEqual(taskRequest.body, { project_id: 'project-1', board_id: 'board-1', title: 'Task', prompt: 'Run' });
    const patchRequest = seen.find((request) => request.method === 'PATCH');
    assert.deepEqual(patchRequest.body, { revision: 2, title: 'Updated', prompt: 'Run now', status: 'in_progress' });
    assert.equal(patchRequest.ifMatch, '"2"');
  } finally {
    await new Promise((resolve) => server.close(resolve));
  }
});
