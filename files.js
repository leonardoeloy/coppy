import { createHash, randomUUID } from 'node:crypto';
import { createReadStream, createWriteStream } from 'node:fs';
import { mkdir, rename, unlink } from 'node:fs/promises';
import { join } from 'node:path';
import { pipeline } from 'node:stream/promises';
import { Transform } from 'node:stream';
import { db } from './db.js';

const root = process.env.COPPY_FILES || new URL('./data/files/', import.meta.url).pathname;
await mkdir(root, { recursive: true });
db.exec('CREATE TABLE IF NOT EXISTS files (path TEXT PRIMARY KEY COLLATE NOCASE, hash TEXT NOT NULL, size INTEGER NOT NULL, updated INTEGER NOT NULL)');
const all = db.prepare('SELECT * FROM files ORDER BY path');
const get = db.prepare('SELECT * FROM files WHERE path = ?');
const put = db.prepare('INSERT INTO files VALUES (?, ?, ?, ?) ON CONFLICT(path) DO UPDATE SET hash=excluded.hash,size=excluded.size,updated=excluded.updated');
export function validPath(p) {
  return typeof p === 'string' && p.length > 0 && p.length <= 1024 && p.split('/').every(s => s && s !== '.' && s !== '..' && !s.startsWith('.coppy-') && !/[\\<>:"|?*\x00-\x1f]/.test(s) && !/[. ]$/.test(s) && !/^(con|prn|aux|nul|com[0-9]|lpt[0-9])(\.|$)/i.test(s));
}
export async function filesHandler(req, res, url, json, broadcast) {
  if (url.pathname === '/api/files' && req.method === 'GET') { json(res, 200, { files: all.all() }); return; }
  const p = url.searchParams.get('path');
  if (!validPath(p)) return json(res, 400, { error: 'Invalid or non-portable file path' });
  if (req.method === 'GET') {
    const row = get.get(p);
    if (!row) return json(res, 404, { error: 'File not found' });
    res.writeHead(200, { 'Content-Type': 'application/octet-stream', 'Content-Length': row.size, 'Content-Disposition': `attachment; filename*=UTF-8''${encodeURIComponent(row.path.split('/').pop())}`, 'X-Content-Type-Options': 'nosniff', ETag: `"${row.hash}"` });
    await pipeline(createReadStream(join(root, row.hash)), res);
    return;
  }
  if (req.method !== 'PUT') return json(res, 405, { error: 'Method not allowed' });
  const expected = req.headers['if-match'];
  if (expected === undefined) return json(res, 428, { error: 'If-Match required (empty string for a new file)' });
  const tmp = join(root, `.upload-${randomUUID()}`);
  let size = 0;
  const hash = createHash('sha256');
  try {
    await pipeline(req, new Transform({ transform(chunk, enc, cb) {
      size += chunk.length;
      if (size > Number(process.env.COPPY_MAX_FILE_BYTES || 10737418240)) return cb(new Error('File too large'));
      hash.update(chunk); cb(null, chunk);
    } }), createWriteStream(tmp, { flags: 'wx' }));
    const digest = hash.digest('hex');
    // Store immutable bytes before the synchronous compare-and-swap.
    await rename(tmp, join(root, digest));
    const current = get.get(p);
    if ((current?.hash || '') !== expected || (current && current.path !== p)) return json(res, 409, { error: 'File changed', file: current });
    if (all.all().some(f => f.path.toLowerCase().startsWith(p.toLowerCase() + '/') || p.toLowerCase().startsWith(f.path.toLowerCase() + '/'))) return json(res, 409, { error: 'File/directory collision' });
    put.run(p, digest, size, Date.now());
    const file = get.get(p);
    broadcast('file', { file });
    json(res, 200, { file });
  } finally { await unlink(tmp).catch(() => {}); }
}
