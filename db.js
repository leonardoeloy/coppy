import { DatabaseSync } from 'node:sqlite';
import { createHash, randomUUID } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
export const db = new DatabaseSync(process.env.COPPY_DB || join(here, 'coppy.db'));

db.exec(`
  PRAGMA journal_mode = WAL;

  CREATE TABLE IF NOT EXISTS devices (
    id          TEXT PRIMARY KEY,
    fingerprint TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    user_agent  TEXT NOT NULL,
    ip          TEXT NOT NULL,
    first_seen  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL
  );

  CREATE TABLE IF NOT EXISTS clips (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id  TEXT NOT NULL REFERENCES devices(id),
    text       TEXT NOT NULL,
    created_at INTEGER NOT NULL
  );

  CREATE INDEX IF NOT EXISTS clips_created_at ON clips (created_at DESC);
`);

const q = {
  byFingerprint: db.prepare('SELECT * FROM devices WHERE fingerprint = ?'),
  byId: db.prepare('SELECT * FROM devices WHERE id = ?'),
  insertDevice: db.prepare(
    `INSERT INTO devices (id, fingerprint, name, user_agent, ip, first_seen, last_seen)
     VALUES (?, ?, ?, ?, ?, ?, ?)`
  ),
  touchDevice: db.prepare('UPDATE devices SET last_seen = ?, ip = ?, user_agent = ? WHERE id = ?'),
  renameDevice: db.prepare('UPDATE devices SET name = ? WHERE id = ?'),
  allDevices: db.prepare('SELECT * FROM devices ORDER BY first_seen ASC'),
  insertClip: db.prepare('INSERT INTO clips (device_id, text, created_at) VALUES (?, ?, ?)'),
  clipById: db.prepare('SELECT * FROM clips WHERE id = ?'),
  recentClips: db.prepare('SELECT * FROM clips ORDER BY id DESC LIMIT ?'),
  deleteClip: db.prepare('DELETE FROM clips WHERE id = ?'),
  clearClips: db.prepare('DELETE FROM clips'),
};

/** Stable-ish device identity derived from IP + User-Agent. */
export function fingerprintOf(ip, userAgent) {
  return createHash('sha256').update(`${ip}\n${userAgent}`).digest('hex').slice(0, 32);
}

/**
 * Resolve the device for a request. A cookie is the primary key (survives IP
 * changes); the ip+UA fingerprint is the fallback so a device that loses its
 * cookie still maps back to the same row.
 */
export function resolveDevice({ cookieId, ip, userAgent }) {
  const now = Date.now();

  let row = cookieId ? q.byId.get(cookieId) : undefined;
  if (!row) row = q.byFingerprint.get(fingerprintOf(ip, userAgent));

  if (row) {
    q.touchDevice.run(now, ip, userAgent, row.id);
    return { ...row, last_seen: now, ip, user_agent: userAgent };
  }

  const device = {
    id: randomUUID(),
    fingerprint: fingerprintOf(ip, userAgent),
    name: describeClient(userAgent, ip),
    user_agent: userAgent,
    ip,
    first_seen: now,
    last_seen: now,
  };
  q.insertDevice.run(
    device.id, device.fingerprint, device.name,
    device.user_agent, device.ip, device.first_seen, device.last_seen
  );
  return device;
}

export function renameDevice(id, name) {
  q.renameDevice.run(name, id);
  return q.byId.get(id);
}

export const listDevices = () => q.allDevices.all();
export const getDevice = (id) => q.byId.get(id);

export function addClip(deviceId, text) {
  const info = q.insertClip.run(deviceId, text, Date.now());
  return q.clipById.get(info.lastInsertRowid);
}

export const recentClips = (limit = 200) => q.recentClips.all(limit).reverse();
export const deleteClip = (id) => q.deleteClip.run(id);
export const clearClips = () => q.clearClips.run();

/** Best-effort "Chrome on macOS" style label from a User-Agent string. */
function describeClient(ua = '', ip = '') {
  const browser =
    /Edg\//.test(ua) ? 'Edge' :
    /OPR\//.test(ua) ? 'Opera' :
    /Firefox\//.test(ua) ? 'Firefox' :
    /Chrome\//.test(ua) ? 'Chrome' :
    /Safari\//.test(ua) ? 'Safari' :
    'Browser';

  const os =
    /Windows NT/.test(ua) ? 'Windows' :
    /Mac OS X|Macintosh/.test(ua) ? 'macOS' :
    /Android/.test(ua) ? 'Android' :
    /iPhone|iPad|iPod/.test(ua) ? 'iOS' :
    /Linux/.test(ua) ? 'Linux' :
    'Unknown OS';

  const shortIp = ip.split(':').pop() || ip;
  return `${browser} on ${os} (${shortIp})`;
}
