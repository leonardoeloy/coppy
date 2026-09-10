# coppy

A shared clipboard and file folder for the machines on your desk. Paste text in the browser, upload and download files, or run the desktop sync client to share a folder automatically.

Zero npm dependencies, just plain `node:http`, Server-Sent Events for the push, and the built-in `node:sqlite` for persistence.

![The coppy UI: connected devices along the top, a paste box, and a history of clips labelled with the device each came from](docs/screenshot.png)

## Run

Requires Node.js 22.5+ (the start command enables its built-in SQLite support). Building desktop clients also requires Go 1.22+ and Bash; downloaded clients run without Node or Go installed.

```bash
npm start
```

It prints a `localhost` URL and a LAN URL. Open the LAN URL on every machine you want to sync (they must be on the same network).

`PORT` and `COPPY_DB` override the port (default `3737`) and database path (default `./coppy.db`).

## How it works

1. Copy something on computer A, switch to the coppy tab, hit Cmd-V / Ctrl+V anywhere on the page.
2. The text is stored and pushed over SSE to every connected browser.
3. Every device *except the sender* writes it to its own system clipboard.
4. The history shows every clip with the device it came from.

Typing into the box and pressing ⌘↵ / Ctrl+↵ (or clicking **Send**) does the same thing without needing a real paste.

### Device identity

No accounts. A device is identified by a `sha256(ip + user-agent)` fingerprint, stored in the `devices` table. A cookie holding the device's UUID is the primary key on later visits, so identity survives an IP change; the fingerprint is the fallback, so a device that loses its cookie still maps back to the same row.

Devices get an auto-generated label like `Chrome on macOS (192.168.1.42)`. Click **this device: …** in the header to rename it — the name is stored and shown on every clip that device ever sent.

Two browsers on the same machine count as two devices (different user-agent), which is handy for testing.

### Clipboard caveats

Writing to the system clipboard without a click is something browsers guard:

- **Chrome / Edge on `localhost` or the LAN IP**: works silently while the tab is focused.
- **Safari, and any unfocused tab**: the silent write is refused. coppy falls back to `execCommand('copy')`, and if that is refused too it shows a green bar at the bottom of the page: click it and the clip goes to your clipboard. A queued clip is also retried automatically the next time the tab regains focus.
- The **auto-copy** checkbox in the header turns automatic clipboard writes off entirely; the history and its per-item **copy** buttons still work.

Serving over plain HTTP on a LAN IP is not a secure context, so `navigator.clipboard` may be unavailable there, the `execCommand` fallback will cover it.

## Layout

| file               | what it does                                       |
| ------------------ | -------------------------------------------------- |
| `server.js`        | HTTP routes, SSE fan-out, device cookie handling   |
| `files.js`       | Streaming file API, content-addressed storage, manifest updates |
| `sync/`           | Go desktop client and platform-specific background launch |
| `scripts/build-sync.sh` | Cross-builds Windows/macOS x64 and ARM64 clients |
| `test/files.test.js` | Isolated API and two-client integration tests |
| `db.js`            | SQLite schema, fingerprinting, queries             |
| `public/index.html`| markup, Tailwind via CDN                           |
| `public/app.js`    | Clipboard UI, file uploads/downloads, desktop download commands |

## API

| method | path              | purpose                              |
| ------ | ----------------- | ------------------------------------ |
| GET    | `/api/state`      | current device, all devices, history  |
| GET    | `/api/events`     | SSE stream (`hello`, `clip`, `presence`, `device`, `clip-deleted`, `cleared`, `file`) |
| POST   | `/api/clips`      | `{ text }` → store and broadcast      |
| DELETE | `/api/clips/:id`  | delete one clip                       |
| DELETE | `/api/clips`      | clear history                         |
| PATCH  | `/api/device`     | `{ name }` → rename this device       |

There is no auth. Keep it on a network you trust.

## File sharing and desktop sync

Build the downloadable clients with Go 1.22+ installed:

```bash
npm run build:sync
npm start
```

The web view has **Upload files**, a download link for every shared file, and **Download desktop sync** with Windows x64/ARM64 and macOS Intel/Apple silicon binaries. Uploaded files with duplicate names get a separate name rather than replacing existing content. The visible download buttons include commands using the server address you opened in the browser. Generated binaries are excluded from Git and live in `dist/` and are served by the Node server; rebuild them after changing Go code.

Keep the Node server running on the Mac (or another LAN machine). Download and run a client on **both** computers, pointing both at that server's IP. For example:

```powershell
# Windows PowerShell
.\coppy-windows-amd64.exe --peer 192.168.1.20 .
# Or select a folder (flags go before the folder):
.\coppy-windows-amd64.exe --peer 192.168.1.20:3737 "C:\Users\Leo\Coppy"
```

```bash
# Apple silicon Mac; use darwin-amd64 for an Intel Mac
chmod +x ./coppy-darwin-arm64
./coppy-darwin-arm64 --peer 192.168.1.20 .
```

Each client connects, then runs in the background. The commands above sync the current directory (`.`). Put files in that directory on either machine: the other machine receives them automatically, including nested directories. This shares the selected folder, **not the entire computer**. Browser uploads also arrive in these folders. `--dir` selects a different directory; `--foreground` keeps the client attached to the terminal; `--once` runs one synchronization pass; `--interval 5s` changes the default three-second polling interval. An explicit HTTP or HTTPS server URL is also accepted.

The startup message prints the PID and log path (`.coppy-sync.log` inside the folder). Stop with `kill PID` on Mac or `Stop-Process -Id PID` in PowerShell. This does not install a login/startup service: launch it again after reboot. If a forced termination leaves `.coppy-lock`, remove that directory only after checking no client is still using that folder. Only run one client per folder.

Sync compares SHA-256 content hashes, uploads changed files, and verifies downloads before replacing local files. It retries failed passes while running continuously. It transfers whole changed files, not rsync block deltas; permissions, timestamps, symlinks, and empty directories are not replicated. Files must have portable Windows/macOS names. Local deletions are restored from the shared copy; renames add a new shared path and restore the old one. Concurrent edits are kept as `.conflict-HASH` copies. Initial different files at the same path also create a conflict copy. Resolve conflict copies manually. Files actively being modified may require another pass.

`COPPY_FILES` selects server blob storage (default `data/files/`), and `COPPY_MAX_FILE_BYTES` sets the upload limit (default 10 GiB). Back up the SQLite database and blob directory together. Stored blobs are immutable; old versions and interrupted-upload leftovers are not automatically garbage collected. The server remains unauthenticated: every client on the trusted LAN can read and share files. Do not expose it directly to the internet. The binaries are unsigned; macOS or Windows may require you to approve running a downloaded binary.

File API:

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/files` | Manifest with path, SHA-256 hash, size, updated time |
| GET | `/api/file?path=...` | Download bytes as an attachment |
| PUT | `/api/file?path=...` | Stream bytes; `If-Match` must be the current hash, or an empty value for a new path; stale writes return 409 |
| GET | `/downloads/coppy-OS-ARCH[.exe]` | Download a built client |

Run `npm test` for API and two-client integration coverage. The build produces all four target binaries without CGO. Cross-compilation is checked locally; Windows background behavior should also be exercised on a Windows machine.

### Sync the current directory

```bash
coppy --peer 192.168.1.20 .
```

This short command assumes you renamed the downloaded executable to `coppy` (`coppy.exe` on Windows) and put it on your PATH. Otherwise use its downloaded filename as shown above.

The positional folder is resolved against your current working directory before the client goes into the background. Any relative or absolute folder works, including quoted paths with spaces. With `--peer` and no folder, the default remains `~/Coppy`. Put flags before the folder; use either a positional folder or `--dir`. The older `coppy --dir folder IP` syntax remains supported.


## Development checks

```bash
npm test
(cd sync && go vet ./...)
npm run build:sync
git diff --check
```

The integration test launches an isolated server with temporary storage and exercises bidirectional edits, conflict copies, binary and empty files, rejected stale writes, path and symlink protection, and `--peer SERVER .` in a background process. It requires Go and runs on macOS/Linux. The live server database is not used by tests.

## Roadmap

- [x] Share text with device identity and automatic clipboard copying.
- [x] Upload and download files in the web view.
- [x] Sync folders both ways with downloadable Windows and macOS clients.
- [ ] Transfer files using modified QR codes via camera.
- [ ] Transfer files using encoded audio streams (speaker-to-mic).
- [ ] Improve mobile support.
