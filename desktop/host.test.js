const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { once } = require('node:events');
const { spawn } = require('node:child_process');
const test = require('node:test');

test('desktop host serves the UI and keeps the API secret on the Unix proxy', async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'kovan-desktop-host-'));
  const socketPath = path.join(directory, 'local-api.sock');
  const secretPath = path.join(directory, 'local-api.secret');
  const secret = crypto.randomBytes(32);
  fs.writeFileSync(secretPath, secret, { mode: 0o600 });
  const seen = { auth: '', ifMatch: '' };
  const upstream = http.createServer((request, response) => {
    seen.auth = request.headers['x-agentbridge-local-auth'] || '';
    seen.ifMatch = request.headers['if-match'] || '';
    response.writeHead(200, { 'Content-Type': 'application/json' });
    response.end(JSON.stringify({ status: 'ok' }));
  });
  await once(upstream.listen(socketPath), 'listening');

  const child = spawn(process.execPath, [path.join(__dirname, 'host.js'), '--socket', socketPath, '--secret', secretPath, '--root', __dirname], { stdio: ['ignore', 'pipe', 'pipe'] });
  let output = '';
  child.stdout.on('data', (chunk) => { output += chunk.toString(); });
  try {
    const address = await waitForURL(child, () => output);
    const index = await fetch(address);
    assert.equal(index.status, 200);
    const cookie = index.headers.get('set-cookie').split(';', 1)[0];
    const health = await fetch(`${address}healthz`, { headers: { Cookie: cookie } });
    assert.equal(health.status, 200);
    assert.deepEqual(await health.json(), { status: 'ok' });
    assert.equal(seen.auth, secret.toString('base64url'));
    const update = await fetch(`${address}v1/tasks/task-1`, {
      method: 'PATCH',
      headers: { Cookie: cookie, 'Content-Type': 'application/json', 'If-Match': '"3"' },
      body: '{}',
    });
    assert.equal(update.status, 200);
    assert.equal(seen.ifMatch, '"3"');
    const unauthorized = await fetch(`${address}healthz`);
    assert.equal(unauthorized.status, 401);
  } finally {
    child.kill('SIGTERM');
    await once(child, 'exit');
    await new Promise((resolve) => upstream.close(resolve));
    fs.rmSync(directory, { recursive: true, force: true });
  }
});

async function waitForURL(child, readOutput) {
  for (;;) {
    const match = readOutput().match(/http:\/\/127\.0\.0\.1:\d+/);
    if (match) return match[0] + '/';
    if (child.exitCode !== null) throw new Error('desktop host exited before listening');
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}
