#!/usr/bin/env node
'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');

const args = parseArgs(process.argv.slice(2));
if (args.help) {
  process.stdout.write('usage: node desktop/host.js --socket <path> --secret <path> [--root <dir>] [--port <port>]\n');
  process.exit(0);
}

const root = resolveDirectory(args.root || __dirname);
const socketPath = requiredAbsolute(args.socket || process.env.AGENTBRIDGE_LOCAL_API_SOCKET, '--socket');
const secretPath = requiredAbsolute(args.secret || process.env.AGENTBRIDGE_LOCAL_API_SECRET, '--secret');
const secret = readSecret(secretPath);
const upstreamSecret = secret.toString('base64url');
const browserToken = crypto.randomBytes(32).toString('base64url');
const cookieName = 'kovan_desktop_session';
const port = args.port === undefined ? 0 : parsePort(args.port);

const server = http.createServer((request, response) => {
  const pathname = new URL(request.url || '/', 'http://127.0.0.1').pathname;
  if (pathname === '/healthz' || pathname.startsWith('/v1/')) {
    if (!authorizedBrowser(request)) {
      writeJSON(response, 401, { error: 'desktop_session_required' });
      return;
    }
    proxyLocalAPI(request, response);
    return;
  }
  serveStatic(request, response, pathname);
});

server.on('error', (error) => {
  process.stderr.write(`kovan desktop host: ${error.message}\n`);
  process.exitCode = 1;
});

server.listen(port, '127.0.0.1', () => {
  const address = server.address();
  process.stdout.write(`kovan desktop host: http://127.0.0.1:${address.port}\n`);
});

function parseArgs(values) {
  const result = {};
  for (let index = 0; index < values.length; index += 1) {
    const value = values[index];
    if (value === '--help' || value === '-h') {
      result.help = true;
      continue;
    }
    if (!['--socket', '--secret', '--root', '--port'].includes(value)) {
      throw new Error(`unknown argument: ${value}`);
    }
    const next = values[index + 1];
    if (!next || next.startsWith('--')) {
      throw new Error(`missing value for ${value}`);
    }
    result[value.slice(2)] = next;
    index += 1;
  }
  return result;
}

function requiredAbsolute(value, flag) {
  if (!value || !path.isAbsolute(value)) {
    throw new Error(`${flag} must be an absolute path`);
  }
  return path.normalize(value);
}

function resolveDirectory(value) {
  const directory = requiredAbsolute(value, '--root');
  const info = fs.lstatSync(directory);
  if (!info.isDirectory() || info.isSymbolicLink()) {
    throw new Error('--root must be a real directory');
  }
  return directory;
}

function readSecret(filename) {
  const info = fs.lstatSync(filename);
  if (!info.isFile() || info.isSymbolicLink() || (info.mode & 0o077) !== 0) {
    throw new Error('--secret must be an owner-only regular file');
  }
  const value = fs.readFileSync(filename);
  if (value.length < 32) {
    throw new Error('--secret is too short');
  }
  return value;
}

function parsePort(value) {
  const parsed = Number(value);
  if (!Number.isInteger(parsed) || parsed < 0 || parsed > 65535) {
    throw new Error('--port must be an integer between 0 and 65535');
  }
  return parsed;
}

function authorizedBrowser(request) {
  const cookies = String(request.headers.cookie || '').split(';');
  const candidate = cookies.map((entry) => entry.trim().split('=')).find(([name]) => name === cookieName)?.[1] || '';
  const left = Buffer.from(candidate);
  const right = Buffer.from(browserToken);
  return left.length === right.length && crypto.timingSafeEqual(left, right);
}

function sessionHeaders() {
  return { 'Set-Cookie': `${cookieName}=${browserToken}; HttpOnly; SameSite=Strict; Path=/` };
}

function serveStatic(request, response, pathname) {
  if (request.method !== 'GET' && request.method !== 'HEAD') {
    writeJSON(response, 405, { error: 'method_not_allowed' });
    return;
  }
  const files = { '/': 'index.html', '/index.html': 'index.html', '/styles.css': 'styles.css', '/app.js': 'app.js', '/README.md': 'README.md' };
  const filename = files[pathname];
  if (!filename) {
    writeJSON(response, 404, { error: 'not_found' });
    return;
  }
  const fullPath = path.join(root, filename);
  let info;
  try {
    info = fs.lstatSync(fullPath);
  } catch (error) {
    if (error.code === 'ENOENT') {
      writeJSON(response, 404, { error: 'not_found' });
      return;
    }
    writeJSON(response, 500, { error: 'static_file_unavailable' });
    return;
  }
  if (!info.isFile() || info.isSymbolicLink()) {
    writeJSON(response, 404, { error: 'not_found' });
    return;
  }
  const contentTypes = { '.html': 'text/html; charset=utf-8', '.css': 'text/css; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.md': 'text/plain; charset=utf-8' };
  response.writeHead(200, { ...sessionHeaders(), 'Content-Type': contentTypes[path.extname(filename)] || 'application/octet-stream', 'Content-Length': info.size, 'Cache-Control': 'no-store' });
  if (request.method === 'HEAD') {
    response.end();
    return;
  }
  fs.createReadStream(fullPath).on('error', () => response.destroy()).pipe(response);
}

function proxyLocalAPI(request, response) {
  const headers = { 'X-AgentBridge-Local-Auth': upstreamSecret };
  if (request.headers['content-type']) headers['content-type'] = request.headers['content-type'];
  if (request.headers['content-length']) headers['content-length'] = request.headers['content-length'];
  if (request.headers.accept) headers.accept = request.headers.accept;
  if (request.headers['if-match']) headers['if-match'] = request.headers['if-match'];
  const upstream = http.request({ socketPath, path: request.url, method: request.method, headers }, (upstreamResponse) => {
    const responseHeaders = { 'Content-Type': upstreamResponse.headers['content-type'] || 'application/json; charset=utf-8', 'Cache-Control': 'no-store' };
    response.writeHead(upstreamResponse.statusCode || 502, responseHeaders);
    upstreamResponse.pipe(response);
  });
  upstream.setTimeout(30000, () => upstream.destroy(new Error('local API timeout')));
  upstream.on('error', (error) => {
    if (!response.headersSent) writeJSON(response, 503, { error: 'local_api_unavailable', detail: error.message });
    else response.destroy();
  });
  request.on('aborted', () => upstream.destroy());
  request.pipe(upstream);
}

function writeJSON(response, status, value) {
  const body = Buffer.from(JSON.stringify(value));
  response.writeHead(status, { ...sessionHeaders(), 'Content-Type': 'application/json; charset=utf-8', 'Content-Length': body.length, 'Cache-Control': 'no-store' });
  response.end(body);
}

process.on('SIGINT', () => server.close(() => process.exit(0)));
process.on('SIGTERM', () => server.close(() => process.exit(0)));
