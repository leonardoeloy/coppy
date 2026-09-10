package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed public/*
var webAssets embed.FS

type subscriber struct {
	id     string
	events chan string
}
type app struct {
	db                        *sql.DB
	filesDir, downloads       string
	ca                        []byte
	maxFile                   int64
	fileMu, identityMu, hubMu sync.Mutex
	streams                   map[*subscriber]bool
	static                    http.Handler
}

func newApp(o options, ca []byte) (*app, error) {
	db, err := openStore(o.DB)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(o.Files, 0700); err != nil {
		db.Close()
		return nil, err
	}
	web, _ := fs.Sub(webAssets, "public")
	a := &app{db: db, filesDir: o.Files, downloads: o.Downloads, ca: ca, maxFile: 10 << 30, streams: map[*subscriber]bool{}, static: http.FileServer(http.FS(web))}
	if v := os.Getenv("COPPY_MAX_FILE_BYTES"); v != "" {
		n, e := strconv.ParseInt(v, 10, 64)
		if e != nil || n <= 0 {
			db.Close()
			return nil, fmt.Errorf("invalid COPPY_MAX_FILE_BYTES")
		}
		a.maxFile = n
	}
	return a, nil
}
func runServer(ctx context.Context, o options) error {
	cert, ca, err := ensureTLS(o.TLSDir)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(o.DB), 0700); err != nil {
		return err
	}
	lock := o.DB + ".lock"
	if err = os.Mkdir(lock, 0700); err != nil {
		return fmt.Errorf("database is locked; if no server is running remove %s", lock)
	}
	defer os.Remove(lock)
	a, err := newApp(o, ca)
	if err != nil {
		return err
	}
	defer a.db.Close()
	listener, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: a, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(tls.NewListener(listener, srv.TLSConfig)) }()
	log.Printf("Coppy HTTPS server: https://%s\nCA SHA-256: %s", listener.Addr(), fingerprint(ca))
	if err = markReady(); err != nil {
		srv.Close()
		<-done
		return err
	}
	select {
	case <-ctx.Done():
		srv.Close()
		<-done
		return nil
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
func respond(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
func failure(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}
func readBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 512<<10)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}
func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := a.route(w, r); err != nil {
		log.Print(err)
		failure(w, 500, "Internal server error")
	}
}

var downloadName = regexp.MustCompile(`^coppy-(windows|darwin)-(amd64|arm64)(\.exe)?$`)

func (a *app) route(w http.ResponseWriter, r *http.Request) error {
	path := r.URL.Path
	if path == "/api/tls" && r.Method == "GET" {
		respond(w, 200, map[string]any{"localCA": true, "embeddedCA": true, "fingerprint": fingerprint(a.ca)})
		return nil
	}
	if path == "/downloads/coppy-ca.pem" && r.Method == "GET" {
		w.Header().Set("Content-Disposition", `attachment; filename="coppy-ca.pem"`)
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Write(a.ca)
		return nil
	}
	if strings.HasPrefix(path, "/downloads/") && r.Method == "GET" {
		name := strings.TrimPrefix(path, "/downloads/")
		var manifest struct {
			Fingerprint string `json:"fingerprint"`
		}
		b, err := os.ReadFile(filepath.Join(a.downloads, ".coppy-ca.json"))
		if !downloadName.MatchString(name) || err != nil || json.Unmarshal(b, &manifest) != nil || manifest.Fingerprint != fingerprint(a.ca) {
			failure(w, 404, "Build downloads for this server with --build-downloads")
			return nil
		}
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, filepath.Join(a.downloads, name))
		return nil
	}
	if path == "/api/files" && r.Method == "GET" {
		files, err := a.files()
		if err != nil {
			return err
		}
		respond(w, 200, map[string]any{"files": files})
		return nil
	}
	if path == "/api/file" {
		return a.file(w, r)
	}
	if path == "/api/events" && r.Method == "GET" {
		d, err := a.identify(w, r)
		if err != nil {
			return err
		}
		a.events(w, r, d)
		return nil
	}
	if path == "/api/state" && r.Method == "GET" {
		d, err := a.identify(w, r)
		if err != nil {
			return err
		}
		devices, err := a.devices()
		if err != nil {
			return err
		}
		clips, err := a.clips()
		if err != nil {
			return err
		}
		respond(w, 200, map[string]any{"me": d, "devices": devices, "clips": clips, "online": a.online()})
		return nil
	}
	if path == "/api/device" && r.Method == "PATCH" {
		d, err := a.identify(w, r)
		if err != nil {
			return err
		}
		var body struct {
			Name string `json:"name"`
		}
		if err = readBody(w, r, &body); err != nil {
			failure(w, 400, "Invalid JSON")
			return nil
		}
		name := []rune(strings.TrimSpace(body.Name))
		if len(name) == 0 {
			failure(w, 400, "empty name")
			return nil
		}
		if len(name) > 60 {
			name = name[:60]
		}
		d.Name = string(name)
		if _, err = a.db.Exec("UPDATE devices SET name=? WHERE id=?", d.Name, d.ID); err != nil {
			return err
		}
		a.broadcast("device", map[string]any{"device": d})
		respond(w, 200, map[string]any{"device": d})
		return nil
	}
	if path == "/api/clips" && r.Method == "POST" {
		d, err := a.identify(w, r)
		if err != nil {
			return err
		}
		var body struct {
			Text string `json:"text"`
		}
		if err = readBody(w, r, &body); err != nil {
			failure(w, 400, "Invalid JSON or payload too large")
			return nil
		}
		if strings.TrimSpace(body.Text) == "" {
			failure(w, 400, "empty text")
			return nil
		}
		if len(body.Text) > 256<<10 {
			failure(w, 413, "text too large")
			return nil
		}
		c := clip{DeviceID: d.ID, Text: body.Text, CreatedAt: time.Now().UnixMilli()}
		result, err := a.db.Exec("INSERT INTO clips(device_id,text,created_at) VALUES(?,?,?)", c.DeviceID, c.Text, c.CreatedAt)
		if err != nil {
			return err
		}
		c.ID, err = result.LastInsertId()
		if err != nil {
			return err
		}
		a.broadcast("clip", map[string]any{"clip": c, "device": d})
		respond(w, 201, map[string]any{"clip": c})
		return nil
	}
	if (path == "/api/clips" || strings.HasPrefix(path, "/api/clips/")) && r.Method == "DELETE" {
		if _, err := a.identify(w, r); err != nil {
			return err
		}
		if path == "/api/clips" {
			if _, err := a.db.Exec("DELETE FROM clips"); err != nil {
				return err
			}
			a.broadcast("cleared", map[string]any{})
		} else {
			id, err := strconv.ParseInt(strings.TrimPrefix(path, "/api/clips/"), 10, 64)
			if err != nil || id < 1 {
				failure(w, 400, "bad id")
				return nil
			}
			if _, err = a.db.Exec("DELETE FROM clips WHERE id=?", id); err != nil {
				return err
			}
			a.broadcast("clip-deleted", map[string]any{"id": id})
		}
		respond(w, 200, map[string]bool{"ok": true})
		return nil
	}
	if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/downloads/") {
		failure(w, 404, "not found")
		return nil
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		failure(w, 405, "Method not allowed")
		return nil
	}
	w.Header().Set("Cache-Control", "no-cache")
	a.static.ServeHTTP(w, r)
	return nil
}
func (a *app) file(w http.ResponseWriter, r *http.Request) error {
	path := r.URL.Query().Get("path")
	if !safe(path) {
		failure(w, 400, "Invalid or non-portable path")
		return nil
	}
	if r.Method == "GET" {
		var f fileEntry
		err := a.db.QueryRow("SELECT path,hash,size,updated FROM files WHERE path=?", path).Scan(&f.Path, &f.Hash, &f.Size, &f.Updated)
		if err == sql.ErrNoRows {
			failure(w, 404, "File not found")
			return nil
		}
		if err != nil {
			return err
		}
		if len(f.Hash) != 64 {
			return fmt.Errorf("invalid stored hash")
		}
		if _, err = hex.DecodeString(f.Hash); err != nil {
			return err
		}
		file, err := os.Open(filepath.Join(a.filesDir, f.Hash))
		if err != nil {
			return err
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(f.Path)}))
		w.Header().Set("ETag", `"`+f.Hash+`"`)
		http.ServeContent(w, r, f.Path, time.UnixMilli(f.Updated), file)
		return nil
	}
	if r.Method != "PUT" {
		failure(w, 405, "Method not allowed")
		return nil
	}
	expected, present := r.Header["If-Match"]
	if !present || len(expected) != 1 {
		failure(w, 428, "If-Match required")
		return nil
	}
	tmp, err := os.CreateTemp(a.filesDir, ".upload-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hash), http.MaxBytesReader(w, r.Body, a.maxFile))
	closeErr := tmp.Close()
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			failure(w, 413, "File too large")
		} else {
			failure(w, 400, "Incomplete upload")
		}
		return nil
	}
	if closeErr != nil {
		return closeErr
	}
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	files, err := a.files()
	if err != nil {
		return err
	}
	var current fileEntry
	for _, f := range files {
		if strings.EqualFold(f.Path, path) {
			current = f
		}
		lower, other := strings.ToLower(path), strings.ToLower(f.Path)
		if strings.HasPrefix(lower, other+"/") || strings.HasPrefix(other, lower+"/") {
			failure(w, 409, "File/directory collision")
			return nil
		}
	}
	if current.Hash != expected[0] || (current.Path != "" && current.Path != path) {
		respond(w, 409, map[string]any{"error": "File changed", "file": current})
		return nil
	}
	h := hex.EncodeToString(hash.Sum(nil))
	dest := filepath.Join(a.filesDir, h)
	if _, err = os.Stat(dest); os.IsNotExist(err) {
		if err = os.Rename(tmp.Name(), dest); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	f := fileEntry{path, h, size, time.Now().UnixMilli()}
	_, err = a.db.Exec("INSERT INTO files(path,hash,size,updated) VALUES(?,?,?,?) ON CONFLICT(path) DO UPDATE SET hash=excluded.hash,size=excluded.size,updated=excluded.updated", f.Path, f.Hash, f.Size, f.Updated)
	if err != nil {
		return err
	}
	a.broadcast("file", map[string]any{"file": f})
	respond(w, 200, map[string]any{"file": f})
	return nil
}
func (a *app) online() []string {
	a.hubMu.Lock()
	defer a.hubMu.Unlock()
	ids := map[string]bool{}
	out := []string{}
	for s := range a.streams {
		if !ids[s.id] {
			ids[s.id] = true
			out = append(out, s.id)
		}
	}
	return out
}
func event(name string, data any) string {
	b, _ := json.Marshal(data)
	return "event: " + name + "\ndata: " + string(b) + "\n\n"
}
func (a *app) broadcast(name string, data any) {
	message := event(name, data)
	a.hubMu.Lock()
	defer a.hubMu.Unlock()
	for s := range a.streams {
		select {
		case s.events <- message:
		default:
			close(s.events)
			delete(a.streams, s)
		}
	}
}
func (a *app) events(w http.ResponseWriter, r *http.Request, d device) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	s := &subscriber{d.ID, make(chan string, 64)}
	a.hubMu.Lock()
	a.streams[s] = true
	a.hubMu.Unlock()
	defer func() {
		a.hubMu.Lock()
		delete(a.streams, s)
		a.hubMu.Unlock()
		a.broadcast("presence", map[string]any{"online": a.online()})
	}()
	controller := http.NewResponseController(w)
	write := func(text string) error {
		controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.WriteString(w, text); err != nil {
			return err
		}
		return controller.Flush()
	}
	if write("retry: 2000\n\n"+event("hello", map[string]any{"device": d})) != nil {
		return
	}
	a.broadcast("presence", map[string]any{"online": a.online()})
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case message, ok := <-s.events:
			if !ok || write(message) != nil {
				return
			}
		case <-ticker.C:
			if write(": ping\n\n") != nil {
				return
			}
		}
	}
}
