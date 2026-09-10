# coppy

A shared clipboard and file folder for the machines on your desk. One Go executable serves the web app while syncing a local folder, or syncs a folder with another Coppy server. No Node.js, browser extension, or runtime installation is required.

## Start a server

```bash
coppy
```

With no `--peer`, Coppy runs an HTTPS server in the foreground at **https://127.0.0.1:3737** and synchronizes the current directory in the same process. Files added or edited there are shared automatically, and files from clients or web uploads appear there. Its web UI is embedded in the executable. It creates a local certificate authority, server certificate, SQLite database, and file storage inside `.coppy-server/` in the current directory.

To make the server reachable from Windows, another Mac, or other LAN machines:

```bash
coppy --listen 0.0.0.0:3737
```

Add `-b` to launch it in the background:

```bash
coppy --listen 0.0.0.0:3737 -b
```

Select another folder with a positional path or `--dir`:

```bash
coppy --listen 0.0.0.0:3737 /path/to/shared-folder
# Equivalent:
coppy --listen 0.0.0.0:3737 --dir /path/to/shared-folder
```

The server's folder uses the same conflict handling and local scan interval as a client. No separate local client process is needed. The default bind address is localhost. LAN access must be enabled explicitly. Keep the server running while clients sync. There is no login or client authorization: anyone who can reach the listener can access the clipboard and shared files. TLS encrypts connections; database and file contents remain unencrypted on disk.

## Sync a folder

Download the executable for your machine from **Download desktop sync** in the web app. These builds embed that server's **public CA certificate**, so no separate certificate file or `--ca` option is required:

```bash
coppy --peer 192.168.1.69 .
```

This syncs the current directory, recursively, in the **foreground**. Use `-b` to detach:

```bash
coppy --peer 192.168.1.69 -b .
```

Flags go before the positional folder. Relative and absolute folders work, including quoted paths with spaces. Omit the folder to use `~/Coppy`, or use `--dir` instead of a positional folder. All clients should point at the same server. The server machine already syncs its selected folder.

The short `coppy` command assumes the binary was renamed and added to your PATH. Using downloaded filenames:

```powershell
.\coppy-windows-amd64.exe --peer 192.168.1.69 -b .
```

```bash
chmod +x ./coppy-darwin-arm64
./coppy-darwin-arm64 --peer 192.168.1.69 -b .
```

`--interval 5s` changes the default three-second local folder scan interval. Remote changes arrive immediately over secure WebSockets (`wss://`), without waiting for that scan. Idle clients do not poll the server manifest. `--once` runs a single pass and exits. Bare addresses use HTTPS on port 3737; an explicit HTTPS origin with a custom port also works. Plain HTTP and redirects are rejected.

Foreground processes stop with Ctrl+C. Background launch prints the PID and `.coppy.log` path. Stop with `kill PID` on macOS or `Stop-Process -Id PID` in PowerShell. There is no login/startup service; launch again after reboot. Forced termination may leave a lock: remove `.coppy-lock` in the sync folder, or the database's `.lock` directory, only after checking that no process is still using it.

### Sync behavior

- Files flow both ways through the server. Web uploads arrive in connected sync folders; every shared file has a browser download link.
- SHA-256 hashes detect content changes. Transfers send whole changed files, verify downloaded content, and replace local files through temporary files.
- Concurrent edits preserve a `.conflict-HASH` copy. Existing different contents on an initial sync also produce a conflict copy.
- Local deletions are restored from the server. Renaming creates a new shared path and restores the old one. There is no distributed delete operation.
- Symlinks, empty directories, permissions, and timestamps are not replicated. File names must work on Windows and macOS. Reserved `.coppy-` files/directories are skipped.
- Browser uploads with duplicate names receive another name. Continuous clients retry failed passes and log errors.

## HTTPS and embedded trust

The server generates a ten-year local CA and a one-year server certificate using Go's cryptography libraries. Certificates cover localhost, the machine hostname, and its current network addresses. Existing identities are reused and never silently replaced. Private keys stay in the server's TLS directory and are **never embedded or downloadable**.

Desktop downloads trust the embedded CA and still verify the certificate chain, server hostname/IP, and expiry. A binary downloaded from server A does not automatically trust a new server B. Use a build made for B, or explicitly supply `--ca /path/to/verified-ca.pem`. Plain `go build` does not embed an installation's CA; those builds use system trust unless `--ca` is supplied.

Browsers cannot use the executable's embedded trust. Obtain the public CA from a trusted channel, compare its SHA-256 fingerprint with the value printed in the server terminal, and import it into your browser/OS trust. The web app also offers the public CA for download. Trust only the verified public certificate; do not bypass an unexpected certificate mismatch. You can inspect a PEM certificate with:

```bash
openssl x509 -in coppy-ca.pem -noout -fingerprint -sha256
```

On Windows, use the current user's Trusted Root Certification Authorities; on macOS, use your login keychain with SSL trust. Browsers with a separate certificate store may require their own import. Obtain the executable itself through a trusted channel too: its embedded certificate is part of the trust setup.

Use `--tls-dir` to select an existing identity directory containing `ca.pem`, `server.pem`, and `server-key.pem`. Keep that directory outside shared folders. If your IP changes or the leaf certificate expires, replace `server.pem` and its matching key with a certificate signed by the same CA and containing the correct IP/DNS names, then restart. Replacing the CA also requires rebuilding downloads and distributing the new public trust. Download routes refuse builds associated with a different CA.

## Build

Requires Go 1.25+ (Go may automatically download a newer toolchain for dependencies). SQLite uses the CGO-free `modernc.org/sqlite` driver. No Node or OpenSSL is needed to run or build Coppy.

```bash
go build -o dist/coppy .
./dist/coppy
```

Build all four server-specific downloads from this source checkout:

```bash
go run . --build-downloads
```

This uses `.coppy-server/tls` by default, generating it if absent, and produces Windows x64/ARM64 and macOS Intel/Apple silicon binaries in `dist/`. Every build contains the public CA and web UI. Use the same `--tls-dir` for building downloads and running the server:

```bash
go run . --build-downloads --tls-dir /private/coppy-tls
./dist/coppy --tls-dir /private/coppy-tls --listen 0.0.0.0:3737
```

`--downloads` changes the output/serving directory. Rebuild after changing Go or web code because both are compiled into the executable. These binaries are unsigned, so the OS may require approval to run them. Starting a downloaded binary as a fresh server generates its own local identity; building downloads for that new identity requires the source checkout and Go.

`make build`, `make downloads`, `make run`, and `make test` are convenience commands. Use `go run . --build-downloads` directly when supplying a custom TLS or output directory.

## Server configuration and local files

| Flag | Default | Purpose |
| --- | --- | --- |
| `--listen` | `127.0.0.1:3737` | HTTPS bind address and port |
| `--dir` | Current directory in server mode; `~/Coppy` in client mode | Normal folder to synchronize; a positional folder also works |
| `--data` | `.coppy-server` | Server data and background-log directory |
| `--db` | `--data` + `/coppy.db` | SQLite database; overrides `COPPY_DB` |
| `--files` | `--data` + `/files` | Internal content-addressed blob storage, not the watched folder; overrides `COPPY_FILES` |
| `--tls-dir` | `--data` + `/tls` | Server certificates/keys; overrides `COPPY_TLS_DIR` |
| `--downloads` | `dist` | Built executables and their CA manifest |

Relative paths resolve from the launch directory before a background process starts. Client logs and sync metadata live inside the selected sync folder; server logs live under `--data`. Run only one server per database and one sync process per folder. A combined server also takes the folder sync lock; stop any old separate client for that folder before starting it.

The server excludes its configured data directory, database and sidecars, blob storage, TLS directory, downloads, and running executable from uploads and local downloads. Its sync folder must not be inside any of those internal paths.

Back up the database and blob directory together, and retain the private TLS identity separately. Old blobs are not automatically garbage-collected. Generated binaries, SQLite files and sidecars, `.coppy-*` state/log/lock files, and private `*-key.pem` files are ignored by Git. Keep custom storage and TLS directories outside shared or source-controlled folders. Ignore rules do not protect files already tracked by Git.

## Upgrading an existing Go installation

Stop the running server before replacing/restarting its executable. Rebuild downloads with the same `--tls-dir`, then launch the new server with the same database, blob, certificate, and download paths. Add `--dir` or a positional folder to select the server's shared folder; without either, it syncs its launch directory.

Stop any separate sync client watching that server folder: server mode now performs both jobs and requires exclusive access to the folder's sync lock. Update client binaries and refresh web tabs together with the server; older SSE clients do not support the WebSocket endpoint. Keep the existing CA to preserve browser and client trust.

The `--files` option is internal blob storage, not the folder you edit. Use `--dir` or the positional folder for normal named files on both server and client.

## Existing Node installation

The Node implementation has been removed. The Go server directly reads the same SQLite tables and content-addressed files; there is no database conversion. Stop the old server before starting Go, and keep a backup of the database and blob storage together. In an existing checkout:

```bash
go run . --build-downloads --tls-dir data/.coppy-tls
./dist/coppy-darwin-arm64 --db coppy.db --files data/files --tls-dir data/.coppy-tls --listen 0.0.0.0:3737
```

Existing clipboard history, device cookies, shared files, and the HTTPS identity are retained. `COPPY_DB`, `COPPY_FILES`, and `COPPY_TLS_DIR` remain defaults for their corresponding flags. `COPPY_MAX_FILE_BYTES` sets the upload limit (default 10 GiB). Use `--listen` for address/port configuration. The old positional peer syntax and `--foreground` flag are replaced by explicit `--peer` and foreground-by-default behavior.

## Web clipboard

Paste text into the web view or type and click **Send**. The server stores it and pushes it to connected browsers over secure WebSockets. Receiving browsers attempt to copy it automatically; browser permissions/focus may require clicking the green copy banner. The **auto-copy** checkbox disables automatic clipboard writes. Devices can be renamed, and individual clips or all clipboard history can be deleted.

## Live connections

Browsers and continuous sync processes keep a secure WebSocket connection to `/api/ws`. Text frames contain JSON envelopes such as `{"event":"file","data":{"file":{"path":"notes.txt","hash":"...","size":123,"updated":0}}}`. Clipboard and file-change notifications use this channel. File bytes, browser actions, and initial/resync manifests continue to use HTTPS.

Connections use the same certificate verification as HTTPS, ping/pong heartbeats, and automatic reconnect with backoff. On reconnect, browsers reload state and clients resync the manifest to recover missed events. Browser connections must come from the same origin; slow consumers are disconnected and recover on reconnect. The old SSE endpoint has been removed. Rebuild/re-download executables and refresh browser tabs after upgrading.

Local folders are scanned at `--interval` to discover disk changes; that timer does not poll the server while files are unchanged. Failed transfers retry at the scan interval. `--once` performs an HTTPS sync pass and exits without maintaining a WebSocket.

## API

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/state` | Current device, devices, recent clips, online devices |
| GET (WebSocket upgrade) | `/api/ws` | WSS events: hello, clip, presence, device, clip-deleted, cleared, file |
| POST | `/api/clips` | Store and broadcast `{ "text": "..." }` |
| DELETE | `/api/clips` or `/api/clips/:id` | Clear clips or delete one |
| PATCH | `/api/device` | Rename with `{ "name": "..." }` |
| GET | `/api/files` | Manifest of paths, hashes, sizes, updated times |
| GET | `/api/file?path=...` | Download file bytes |
| PUT | `/api/file?path=...` | Upload bytes; `If-Match` is the current hash or empty for a new path; stale writes return 409 |
| GET | `/api/tls` | Public CA fingerprint and embedded-trust metadata |
| GET | `/downloads/coppy-ca.pem` | Public CA certificate |
| GET | `/downloads/coppy-OS-ARCH[.exe]` | Server-specific executable |

## Tests

```bash
go test -race ./...
go vet ./...
```

Tests cover TLS verification, embedded trust, client/server process modes, combined server folder sync, internal-file protection, background startup failures, clipboard/WebSocket behavior, reconnect recovery, idle-network checks, persistent storage, bidirectional sync, conflicts, binary/empty files, and path/symlink protection. Process tests run on macOS/Linux; Windows builds are cross-compiled and still need native Windows execution testing.


## Source layout

| Path | Purpose |
| --- | --- |
| `main.go`, `background_*.go` | CLI modes, process startup, platform detachment |
| `server.go`, `store.go` | HTTPS routes, clipboard/WebSocket, SQLite, file transfers |
| `server_tls.go` | Local identity generation and loading |
| `client.go`, `client_tls.go`, `client_events.go` | Folder sync, WSS events, and embedded/explicit CA verification |
| `build.go` | Cross-platform downloads with embedded public CA |
| `public/` | Web app assets embedded at build time |
| `*_test.go` | TLS, API, storage, sync, and process tests |

Go dependencies are pinned in `go.mod` and `go.sum`.
