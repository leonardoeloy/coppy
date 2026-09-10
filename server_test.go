package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testApp(t *testing.T) (*app, *httptest.Server, *http.Client, options) {
	t.Helper()
	dir := t.TempDir()
	o := options{Data: dir, DB: filepath.Join(dir, "coppy.db"), Files: filepath.Join(dir, "files"), TLSDir: filepath.Join(dir, "tls"), Downloads: filepath.Join(dir, "downloads")}
	cert, ca, err := ensureTLS(o.TLSDir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := newApp(o, ca)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(a)
	s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	s.StartTLS()
	c, err := secureClient(filepath.Join(o.TLSDir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	c.Jar, _ = cookiejar.New(nil)
	t.Cleanup(func() { c.CloseIdleConnections(); s.Close(); a.db.Close() })
	return a, s, c, o
}
func request(t *testing.T, c *http.Client, method, url string, body []byte, match *string) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if match != nil {
		req.Header.Set("If-Match", *match)
	}
	r, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return r.StatusCode, b, r.Header
}
func TestFilesAndSync(t *testing.T) {
	a, s, c, o := testApp(t)
	empty := ""
	put := func(p, text, hash string) int {
		status, _, _ := request(t, c, "PUT", s.URL+"/api/file?path="+p, []byte(text), &hash)
		return status
	}
	for _, p := range []string{"../escape", "CON.txt", ".coppy-secret/key", "folder/a."} {
		if got := put(p, "bad", ""); got != 400 {
			t.Fatalf("%s: %d", p, got)
		}
	}
	if got := put("folder/a.txt", "initial", ""); got != 200 {
		t.Fatal(got)
	}
	for _, p := range []string{"folder/a.txt", "FOLDER/A.txt", "folder"} {
		if got := put(p, "overwrite", ""); got != 409 {
			t.Fatalf("%s: %d", p, got)
		}
	}
	status, _, _ := request(t, c, "PUT", s.URL+"/api/file?path=missing", nil, nil)
	if status != 428 {
		t.Fatal(status)
	}
	a.maxFile = 4
	if got := put("large", "12345", ""); got != 413 {
		t.Fatal(got)
	}
	a.maxFile = 10 << 30
	dirA, dirB := t.TempDir(), t.TempDir()
	syncDir := func(dir string) {
		t.Helper()
		if err := runSync(context.Background(), s.URL, dir, filepath.Join(o.TLSDir, "ca.pem"), true, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	read := func(dir, path, want string) {
		t.Helper()
		b, e := os.ReadFile(filepath.Join(dir, path))
		if e != nil || string(b) != want {
			t.Fatalf("read %s: %q, %v", path, b, e)
		}
	}
	write := func(dir, path, text string) {
		t.Helper()
		if e := os.WriteFile(filepath.Join(dir, path), []byte(text), 0600); e != nil {
			t.Fatal(e)
		}
	}
	syncDir(dirA)
	syncDir(dirB)
	read(dirB, "folder/a.txt", "initial")
	write(dirA, "folder/a.txt", "mac edit")
	syncDir(dirA)
	syncDir(dirB)
	read(dirB, "folder/a.txt", "mac edit")
	write(dirB, "win.txt", "windows")
	syncDir(dirB)
	syncDir(dirA)
	read(dirA, "win.txt", "windows")
	write(dirA, "folder/a.txt", "mac conflict")
	write(dirB, "folder/a.txt", "win conflict")
	syncDir(dirA)
	syncDir(dirB)
	syncDir(dirA)
	read(dirB, "folder/a.txt", "mac conflict")
	names, _ := os.ReadDir(filepath.Join(dirA, "folder"))
	found := false
	for _, n := range names {
		if strings.Contains(n.Name(), ".conflict-") {
			read(dirA, filepath.Join("folder", n.Name()), "win conflict")
			found = true
		}
	}
	if !found {
		t.Fatal("missing conflict")
	}
	binary := []byte{0, 255, 128, 0}
	status, _, _ = request(t, c, "PUT", s.URL+"/api/file?path=binary", binary, &empty)
	if status != 200 {
		t.Fatal(status)
	}
	put("empty", "", "")
	syncDir(dirA)
	read(dirA, "empty", "")
	read(dirA, "binary", string(binary))
	if err := os.Remove(filepath.Join(dirB, "win.txt")); err != nil {
		t.Fatal(err)
	}
	syncDir(dirB)
	read(dirB, "win.txt", "windows")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dirB, "linked")); err != nil {
		t.Fatal(err)
	}
	put("linked/escape.txt", "blocked", "")
	if err := runSync(context.Background(), s.URL, dirB, filepath.Join(o.TLSDir, "ca.pem"), true, time.Second); err == nil {
		t.Fatal("accepted symlink")
	}
	files, _ := os.ReadDir(outside)
	if len(files) != 0 {
		t.Fatal("wrote outside root")
	}
	if _, err := os.Stat(filepath.Join(dirB, ".coppy-lock")); !os.IsNotExist(err) {
		t.Fatal("lock leaked on failure")
	}
}
func TestClipboardWebAndPersistence(t *testing.T) {
	a, s, c, o := testApp(t)
	status, b, _ := request(t, c, "GET", s.URL+"/", nil, nil)
	if status != 200 || !bytes.Contains(b, []byte("Download Windows")) {
		t.Fatal("missing embedded UI")
	}
	status, b, headers := request(t, c, "GET", s.URL+"/api/state", nil, nil)
	if status != 200 || !strings.Contains(headers.Get("Set-Cookie"), "Secure") {
		t.Fatal(status, headers)
	}
	var initial struct {
		Me    device
		Clips []clip
	}
	if err := json.Unmarshal(b, &initial); err != nil {
		t.Fatal(err)
	}
	status, _, _ = request(t, c, "PATCH", s.URL+"/api/device", []byte(`{"name":"Desk"}`), nil)
	if status != 200 {
		t.Fatal(status)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", s.URL+"/api/events", nil)
	events, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Body.Close()
	lines := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		all := ""
		for {
			n, e := events.Body.Read(buf)
			all += string(buf[:n])
			if strings.Contains(all, "event: clip\n") {
				lines <- all
				return
			}
			if e != nil {
				return
			}
		}
	}()
	status, b, _ = request(t, c, "POST", s.URL+"/api/clips", []byte(`{"text":"hello Go"}`), nil)
	if status != 201 {
		t.Fatal(status, string(b))
	}
	var result struct{ Clip clip }
	json.Unmarshal(b, &result)
	select {
	case text := <-lines:
		if !strings.Contains(text, "hello Go") {
			t.Fatal(text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing SSE")
	}
	cancel()
	events.Body.Close()
	second, err := newApp(o, a.ca)
	if err != nil {
		t.Fatal(err)
	}
	defer second.db.Close()
	clips, err := second.clips()
	if err != nil || len(clips) != 1 || clips[0].Text != "hello Go" {
		t.Fatal(clips, err)
	}
	devices, _ := second.devices()
	if len(devices) != 1 || devices[0].Name != "Desk" || devices[0].ID != initial.Me.ID {
		t.Fatal(devices)
	}
	status, _, _ = request(t, c, "DELETE", fmt.Sprintf("%s/api/clips/%d", s.URL, result.Clip.ID), nil, nil)
	if status != 200 {
		t.Fatal(status)
	}
	status, b, _ = request(t, c, "GET", s.URL+"/downloads/coppy-ca.pem", nil, nil)
	if status != 200 || !bytes.Equal(b, a.ca) {
		t.Fatal("wrong CA")
	}
	status, _, _ = request(t, c, "GET", s.URL+"/downloads/coppy-windows-amd64.exe", nil, nil)
	if status != 404 {
		t.Fatal("served unavailable build")
	}
	status, _, _ = request(t, c, "GET", s.URL+"/server-key.pem", nil, nil)
	if status != 404 {
		t.Fatal("key exposed")
	}
}
