import http from 'node:http';
import { readFile } from 'node:fs/promises';
import { networkInterfaces } from 'node:os';
import { fileURLToPath } from 'node:url';
import { dirname, join, normalize } from 'node:path';

import {
  resolveDevice, renameDevice, listDevices, getDevice,
  addClip, recentClips, deleteClip, clearClips,
} from './db.js';

import { filesHandler } from './files.js';

const here = dirname(fileURLToPath(import.meta.url));
const PUBLIC = join(here, 'public');
const PORT = Number(process.env.PORT) || 3737;
const COOKIE = 'coppy_did';
const MAX_TEXT = 256 * 1024; // 256 KB per clip

/** @type {Map<string, Set<http.ServerResponse>>} deviceId -> open SSE streams */
const streams = new Map();

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, `http://${req.headers.host || 'localhost'}`);
  const path = url.pathname;

  try {
    if (path === '/api/files' || path === '/api/file') return await filesHandler(req, res, url, json, broadcast);
    if (path.startsWith('/downloads/') && req.method === 'GET') {
      const name = path.slice('/downloads/'.length);
      if (!/^coppy-(windows|darwin)-(amd64|arm64)(\.exe)?$/.test(name)) return json(res, 404, { error: 'not found' });
      try {
        const data = await readFile(join(here, 'dist', name));
        res.writeHead(200, { 'Content-Type': 'application/octet-stream', 'Content-Disposition': `attachment; filename="${name}"` });
        return res.end(data);
      } catch { return json(res, 404, { error: 'Run npm run build:sync to build downloads' }); }
    }
    if (path === '/api/events') return sseHandler(req, res);
    if (path === '/api/state' && req.method === 'GET') return stateHandler(req, res);
    if (path === '/api/clips' && req.method === 'POST') return postClip(req, res);
    if (path === '/api/clips' && req.method === 'DELETE') return clearHandler(req, res);
    if (path.startsWith('/api/clips/') && req.method === 'DELETE') return deleteHandler(req, res, path);
    if (path === '/api/device' && req.method === 'PATCH') return renameHandler(req, res);
    if (path.startsWith('/api/')) return json(res, 404, { error: 'not found' });
    return serveStatic(req, res, path);
  } catch (err) {
    console.error(err);
    if (!res.headersSent) json(res, 500, { error: String(err.message || err) });
  }
});

// ---------------------------------------------------------------- handlers

function stateHandler(req, res) {
  const me = identify(req, res);
  json(res, 200, {
    me: publicDevice(me),
    devices: listDevices().map(publicDevice),
    clips: recentClips().map(publicClip),
    online: [...streams.keys()],
  });
}

function sseHandler(req, res) {
  const me = identify(req, res);

  res.writeHead(200, {
    'Content-Type': 'text/event-stream',
    'Cache-Control': 'no-cache, no-transform',
    Connection: 'keep-alive',
    'X-Accel-Buffering': 'no',
  });
  res.write('retry: 2000\n\n');

  const wasOffline = !streams.has(me.id);
  if (wasOffline) streams.set(me.id, new Set());
  streams.get(me.id).add(res);

  send(res, 'hello', { device: publicDevice(me) });
  if (wasOffline) broadcastPresence();

  const beat = setInterval(() => res.write(': ping\n\n'), 25_000);

  req.on('close', () => {
    clearInterval(beat);
    const set = streams.get(me.id);
    if (!set) return;
    set.delete(res);
    if (set.size === 0) {
      streams.delete(me.id);
      broadcastPresence();
    }
  });
}

async function postClip(req, res) {
  const me = identify(req, res);
  const body = await readJson(req);
  const text = typeof body.text === 'string' ? body.text : '';

  if (!text.trim()) return json(res, 400, { error: 'empty text' });
  if (text.length > MAX_TEXT) return json(res, 413, { error: 'text too large' });

  const clip = publicClip(addClip(me.id, text));
  broadcast('clip', { clip, device: publicDevice(getDevice(me.id)) });
  json(res, 201, { clip });
}

function deleteHandler(req, res, path) {
  identify(req, res);
  const id = Number(path.split('/').pop());
  if (!Number.isInteger(id)) return json(res, 400, { error: 'bad id' });
  deleteClip(id);
  broadcast('clip-deleted', { id });
  json(res, 200, { ok: true });
}

function clearHandler(req, res) {
  identify(req, res);
  clearClips();
  broadcast('cleared', {});
  json(res, 200, { ok: true });
}

async function renameHandler(req, res) {
  const me = identify(req, res);
  const { name } = await readJson(req);
  const clean = String(name || '').trim().slice(0, 60);
  if (!clean) return json(res, 400, { error: 'empty name' });

  const device = publicDevice(renameDevice(me.id, clean));
  broadcast('device', { device });
  json(res, 200, { device });
}

// ---------------------------------------------------------------- identity

function identify(req, res) {
  const ip = clientIp(req);
  const userAgent = req.headers['user-agent'] || 'unknown';
  const device = resolveDevice({ cookieId: cookies(req)[COOKIE], ip, userAgent });

  res.setHeader(
    'Set-Cookie',
    `${COOKIE}=${device.id}; Path=/; Max-Age=31536000; SameSite=Lax`
  );
  return device;
}

function clientIp(req) {
  const fwd = req.headers['x-forwarded-for'];
  const raw = fwd ? String(fwd).split(',')[0].trim() : req.socket.remoteAddress || '';
  return raw.replace(/^::ffff:/, '');
}

function cookies(req) {
  return Object.fromEntries(
    (req.headers.cookie || '')
      .split(';')
      .map((part) => part.trim().split('='))
      .filter(([k, v]) => k && v !== undefined)
      .map(([k, ...v]) => [k, decodeURIComponent(v.join('='))])
  );
}

// ---------------------------------------------------------------- broadcast

function send(res, event, data) {
  res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
}

function broadcast(event, data) {
  for (const set of streams.values()) for (const res of set) send(res, event, data);
}

const broadcastPresence = () => broadcast('presence', { online: [...streams.keys()] });

// ---------------------------------------------------------------- shaping

const publicDevice = (d) =>
  d && {
    id: d.id,
    name: d.name,
    fingerprint: d.fingerprint,
    userAgent: d.user_agent,
    ip: d.ip,
    firstSeen: d.first_seen,
    lastSeen: d.last_seen,
  };

const publicClip = (c) => ({
  id: c.id,
  deviceId: c.device_id,
  text: c.text,
  createdAt: c.created_at,
});

// ---------------------------------------------------------------- plumbing

function json(res, status, data) {
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(data));
}

function readJson(req) {
  return new Promise((resolve, reject) => {
    let body = '';
    req.on('data', (chunk) => {
      body += chunk;
      if (body.length > MAX_TEXT * 2) {
        reject(new Error('payload too large'));
        req.destroy();
      }
    });
    req.on('end', () => {
      try {
        resolve(body ? JSON.parse(body) : {});
      } catch {
        reject(new Error('invalid JSON'));
      }
    });
    req.on('error', reject);
  });
}

const TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.svg': 'image/svg+xml',
};

async function serveStatic(req, res, path) {
  const rel = path === '/' ? '/index.html' : path;
  const file = join(PUBLIC, normalize(rel).replace(/^(\.\.[/\\])+/, ''));
  if (!file.startsWith(PUBLIC)) return json(res, 403, { error: 'forbidden' });

  try {
    const data = await readFile(file);
    const ext = file.slice(file.lastIndexOf('.'));
    res.writeHead(200, {
      'Content-Type': TYPES[ext] || 'application/octet-stream',
      'Cache-Control': 'no-cache',
    });
    res.end(data);
  } catch {
    json(res, 404, { error: 'not found' });
  }
}

// ---------------------------------------------------------------- boot

server.listen(PORT, '0.0.0.0', () => {
  console.log(`\n  coppy — shared clipboard and files\n`);
  console.log(`  local:   http://localhost:${PORT}`);
  for (const [, addrs] of Object.entries(networkInterfaces())) {
    for (const a of addrs || []) {
      if (a.family === 'IPv4' && !a.internal) console.log(`  network: http://${a.address}:${PORT}`);
    }
  }
  console.log('');
});
