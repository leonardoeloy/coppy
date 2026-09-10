'use strict';

const $ = (id) => document.getElementById(id);
const el = {
  conn: $('conn'), devices: $('devices'), history: $('history'), empty: $('empty'),
  input: $('input'), send: $('send'), clear: $('clear'), rename: $('rename'),
  meName: $('me-name'), autocopy: $('autocopy'), toast: $('toast'),
  banner: $('banner'), bannerSrc: $('banner-src'),
};

const state = {
  me: null,
  devices: new Map(),
  clips: [],
  online: new Set(),
  /** Last text this tab put on the clipboard or sent — used to break echo loops. */
  lastText: '',
  /** Clip waiting for a user gesture because the browser refused a silent write. */
  pending: null,
};

// ------------------------------------------------------------------ startup

init();

async function init() {
  el.autocopy.checked = localStorage.getItem('coppy.autocopy') !== 'off';
  el.autocopy.addEventListener('change', () =>
    localStorage.setItem('coppy.autocopy', el.autocopy.checked ? 'on' : 'off'));

  await loadState();
  connect();
  wireInput();
}

async function loadState() {
  const data = await (await fetch('/api/state')).json();
  state.me = data.me;
  state.devices = new Map(data.devices.map((d) => [d.id, d]));
  state.clips = data.clips;
  state.online = new Set(data.online);
  el.meName.textContent = state.me.name;
  renderDevices();
  renderHistory();
}

// ------------------------------------------------------------------ realtime

function connect() {
  const es = new EventSource('/api/events');

  es.addEventListener('open', () => setConn('live', 'text-emerald-400 border-emerald-800'));
  es.addEventListener('error', () => setConn('reconnecting…', 'text-amber-400 border-amber-900'));

  es.addEventListener('hello', (e) => {
    state.me = JSON.parse(e.data).device;
    el.meName.textContent = state.me.name;
    setConn('live', 'text-emerald-400 border-emerald-800');
  });

  es.addEventListener('presence', (e) => {
    state.online = new Set(JSON.parse(e.data).online);
    renderDevices();
  });

  es.addEventListener('device', (e) => {
    const { device } = JSON.parse(e.data);
    state.devices.set(device.id, device);
    if (device.id === state.me.id) el.meName.textContent = device.name;
    renderDevices();
    renderHistory();
  });

  es.addEventListener('clip', (e) => {
    const { clip, device } = JSON.parse(e.data);
    state.devices.set(device.id, device);
    state.clips.push(clip);
    renderDevices();
    renderHistory();

    // Everyone except the sender gets it on their real clipboard.
    if (clip.deviceId !== state.me.id && el.autocopy.checked) receive(clip);
  });

  es.addEventListener('clip-deleted', (e) => {
    const { id } = JSON.parse(e.data);
    state.clips = state.clips.filter((c) => c.id !== id);
    renderHistory();
  });

  es.addEventListener('cleared', () => {
    state.clips = [];
    renderHistory();
  });
}

function setConn(text, classes) {
  el.conn.textContent = text;
  el.conn.className = `rounded-full border px-2 py-0.5 text-xs ${classes}`;
}

// ------------------------------------------------------------------ sending

function wireInput() {
  // A real paste anywhere on the page is the primary gesture.
  document.addEventListener('paste', (e) => {
    const text = (e.clipboardData || window.clipboardData)?.getData('text') || '';
    if (!text.trim()) return;
    e.preventDefault();
    el.input.value = text;
    push(text);
  });

  el.send.addEventListener('click', () => push(el.input.value));
  el.input.addEventListener('keydown', (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') push(el.input.value);
  });

  el.clear.addEventListener('click', async () => {
    if (!state.clips.length || !confirm('Delete the entire history?')) return;
    await fetch('/api/clips', { method: 'DELETE' });
  });

  el.rename.addEventListener('click', async () => {
    const name = prompt('Name this device', state.me.name);
    if (!name || !name.trim()) return;
    await fetch('/api/device', {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: name.trim() }),
    });
  });

  el.banner.addEventListener('click', flushPending);
  window.addEventListener('focus', flushPending);
}

async function push(text) {
  text = (text || '').replace(/\r\n/g, '\n');
  if (!text.trim()) return;

  // Don't bounce back what another device just wrote into our clipboard.
  if (text === state.lastText) {
    toast('Already synced');
    el.input.value = '';
    return;
  }

  state.lastText = text;
  el.send.disabled = true;
  try {
    const res = await fetch('/api/clips', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text }),
    });
    if (!res.ok) throw new Error((await res.json()).error || res.statusText);
    el.input.value = '';
    toast('Sent to other devices');
  } catch (err) {
    toast(`Failed: ${err.message}`);
  } finally {
    el.send.disabled = false;
  }
}

// ------------------------------------------------------------------ receiving

async function receive(clip) {
  state.lastText = clip.text;
  if (await writeClipboard(clip.text)) {
    toast(`Copied from ${deviceName(clip.deviceId)}`);
    hideBanner();
  } else {
    // Browsers refuse silent writes when the tab is unfocused, or when the page
    // isn't a secure context. Hold it until the user gives us a gesture.
    state.pending = clip;
    el.bannerSrc.textContent = deviceName(clip.deviceId);
    el.banner.classList.remove('hidden');
  }
}

async function flushPending() {
  if (!state.pending) return;
  const clip = state.pending;
  if (await writeClipboard(clip.text)) {
    state.pending = null;
    hideBanner();
    toast(`Copied from ${deviceName(clip.deviceId)}`);
  }
}

const hideBanner = () => el.banner.classList.add('hidden');

/** Async Clipboard API where available, hidden-textarea + execCommand elsewhere. */
async function writeClipboard(text) {
  if (navigator.clipboard?.writeText && document.hasFocus()) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch { /* fall through to the legacy path */ }
  }

  const ta = document.createElement('textarea');
  ta.value = text;
  ta.setAttribute('readonly', '');
  ta.style.cssText = 'position:fixed;top:0;left:0;opacity:0;pointer-events:none';
  document.body.appendChild(ta);
  ta.select();
  ta.setSelectionRange(0, text.length);
  let ok = false;
  try { ok = document.execCommand('copy'); } catch { ok = false; }
  ta.remove();
  return ok;
}

// ------------------------------------------------------------------ rendering

function renderDevices() {
  el.devices.innerHTML = '';
  for (const d of state.devices.values()) {
    const online = state.online.has(d.id);
    const chip = document.createElement('div');
    chip.className =
      'group flex items-center gap-2 rounded-lg border border-zinc-800 bg-zinc-900/40 px-2.5 py-1.5 text-xs';
    chip.title = `${d.userAgent}\nIP ${d.ip}\nfingerprint ${d.fingerprint}`;
    chip.innerHTML = `
      <span class="h-2 w-2 rounded-full ${online ? 'bg-emerald-500' : 'bg-zinc-700'}"></span>
      <span class="${color(d.id).text} font-medium">${escapeHtml(d.name)}</span>
      ${d.id === state.me.id ? '<span class="text-zinc-600">(you)</span>' : ''}`;
    el.devices.appendChild(chip);
  }
}

function renderHistory() {
  el.history.innerHTML = '';
  const clips = [...state.clips].reverse();
  el.empty.classList.toggle('hidden', clips.length > 0);

  for (const clip of clips) {
    const c = color(clip.deviceId);
    const li = document.createElement('li');
    li.className = 'rounded-xl border border-zinc-800 bg-zinc-900/40 p-3';
    li.innerHTML = `
      <div class="mb-1.5 flex items-center gap-2 text-xs">
        <span class="h-1.5 w-1.5 rounded-full ${c.bg}"></span>
        <span class="${c.text} font-medium">${escapeHtml(deviceName(clip.deviceId))}</span>
        ${clip.deviceId === state.me.id ? '<span class="text-zinc-600">(you)</span>' : ''}
        <time class="text-zinc-600">${when(clip.createdAt)}</time>
        <button data-act="copy" class="ml-auto text-zinc-500 hover:text-emerald-400">copy</button>
        <button data-act="del" class="text-zinc-600 hover:text-red-400">delete</button>
      </div>
      <pre class="max-h-32 overflow-auto whitespace-pre-wrap break-words font-mono text-sm text-zinc-300">${escapeHtml(clip.text)}</pre>`;

    li.querySelector('[data-act="copy"]').addEventListener('click', async () => {
      state.lastText = clip.text;
      toast((await writeClipboard(clip.text)) ? 'Copied' : 'Copy blocked by browser');
    });
    li.querySelector('[data-act="del"]').addEventListener('click', () =>
      fetch(`/api/clips/${clip.id}`, { method: 'DELETE' }));

    el.history.appendChild(li);
  }
}

// ------------------------------------------------------------------ helpers

const deviceName = (id) => state.devices.get(id)?.name || 'unknown device';

const PALETTE = [
  { text: 'text-emerald-400', bg: 'bg-emerald-400' },
  { text: 'text-sky-400', bg: 'bg-sky-400' },
  { text: 'text-violet-400', bg: 'bg-violet-400' },
  { text: 'text-amber-400', bg: 'bg-amber-400' },
  { text: 'text-rose-400', bg: 'bg-rose-400' },
  { text: 'text-teal-400', bg: 'bg-teal-400' },
];

function color(id = '') {
  let h = 0;
  for (const ch of id) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  return PALETTE[h % PALETTE.length];
}

function when(ts) {
  const s = Math.round((Date.now() - ts) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return new Date(ts).toLocaleString();
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (ch) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]));
}

let toastTimer;
function toast(msg) {
  el.toast.textContent = msg;
  el.toast.classList.remove('hidden');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.toast.classList.add('hidden'), 2000);
}

setInterval(renderHistory, 60_000); // keep relative timestamps honest

// File names are rendered as text, never HTML.
async function loadFiles() {
  try {
    const res = await fetch('/api/files');
    if (!res.ok) throw new Error('Could not load files');
    const { files } = await res.json();
    $('files').replaceChildren(...files.map(file => {
      const li = document.createElement('li');
      const a = document.createElement('a');
      a.href = '/api/file?path=' + encodeURIComponent(file.path);
      a.download = file.path.split('/').pop();
      a.className = 'text-emerald-400 hover:underline break-all';
      a.textContent = `${file.path} (${new Intl.NumberFormat().format(file.size)} bytes) ↓`;
      li.append(a); return li;
    }));
  } catch (err) { $('file-status').textContent = err.message; }
}
$('file-upload').addEventListener('change', async (event) => {
  const input = event.target;
  input.disabled = true;
  try {
    for (const file of input.files) {
      $('file-status').textContent = `Uploading ${file.name}…`;
      // Uploads never silently replace another file. Keep both under distinct names.
      let name = file.name;
      for (let attempt = 0; attempt < 2; attempt++) {
        const res = await fetch('/api/file?path=' + encodeURIComponent(name), {
          method: 'PUT', headers: { 'If-Match': '' }, body: file,
        });
        if (res.ok) break;
        if (res.status === 409 && attempt === 0) { name = `${Date.now()}-${file.name}`; continue; }
        throw new Error((await res.json()).error || 'Upload failed');
      }
    }
    $('file-status').textContent = 'Uploaded. Connected desktop clients will sync automatically.';
    await loadFiles();
  } catch (err) { $('file-status').textContent = err.message; }
  finally { input.disabled = false; input.value = ''; }
});
loadFiles();
setInterval(loadFiles, 5000);

// Downloaded executables embed this server’s public CA.
async function loadTLSSetup() {
  try {
    const res = await fetch('/api/tls');
    if (!res.ok) throw new Error('Could not load certificate setup. Reload to try again.');
    const tls = await res.json();
    const syncPeer = JSON.stringify(window.location.origin);
    $('windows-sync-command').textContent = `.\\coppy-windows-amd64.exe --peer ${syncPeer} .`;
    $('mac-sync-command').textContent = `chmod +x coppy-darwin-arm64 && ./coppy-darwin-arm64 --peer ${syncPeer} .`;
    $('ca-download').classList.toggle('hidden', !tls.localCA);
    $('tls-setup').textContent = tls.localCA
      ? 'Desktop downloads include this server’s public certificate—no --ca file needed. Your browser still needs to trust the public CA. Compare its fingerprint with the server terminal before trusting it.'
      : 'HTTPS enabled. The client verifies the server certificate.';
    $('ca-fingerprint').textContent = tls.fingerprint ? `CA SHA-256: ${tls.fingerprint}` : '';
  } catch (err) { $('tls-setup').textContent = err.message; }
}
loadTLSSetup();
