package main

import (
	"context"
	"crypto/tls"
	"github.com/coder/websocket"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebSocketPushReconnectAndIdle(t *testing.T) {
	a, _, client, o := testApp(t)
	cert, _, err := ensureTLS(o.TLSDir)
	if err != nil {
		t.Fatal(err)
	}
	var manifests atomic.Int64
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/files" {
			manifests.Add(1)
		}
		a.ServeHTTP(w, r)
	}))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	s.StartTLS()
	defer s.Close()
	// Foreign browser origins are rejected, and the old SSE endpoint is gone.
	req, _ := http.NewRequest("GET", s.URL+"/api/ws", nil)
	req.Header.Set("Origin", "https://untrusted.example")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: %d", res.StatusCode)
	}
	status, _, _ := request(t, client, "GET", s.URL+"/api/events", nil, nil)
	if status != 404 {
		t.Fatal("SSE still active")
	}
	// WSS uses the same strict trust rules as HTTPS.
	untrusted, _ := secureClient("")
	if conn, _, err := websocket.Dial(context.Background(), "wss://"+strings.TrimPrefix(s.URL, "https://")+"/api/ws", &websocket.DialOptions{HTTPClient: untrusted}); err == nil {
		conn.CloseNow()
		t.Fatal("untrusted WSS accepted")
	}
	folder := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// A one-hour local scan interval makes timely remote updates possible only by push.
	go func() { done <- runSync(ctx, s.URL, folder, filepath.Join(o.TLSDir, "ca.pem"), false, time.Hour) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("WebSocket sync did not shut down")
		}
	}()
	until := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("timed out")
	}
	until(func() bool { a.hubMu.Lock(); defer a.hubMu.Unlock(); return len(a.streams) > 0 })
	put := func(name, text string) {
		t.Helper()
		empty := ""
		status, _, _ := request(t, client, "PUT", s.URL+"/api/file?path="+name, []byte(text), &empty)
		if status != 200 {
			t.Fatal(status)
		}
	}
	put("pushed.txt", "instant")
	until(func() bool { b, _ := os.ReadFile(filepath.Join(folder, "pushed.txt")); return string(b) == "instant" })
	// Break the socket. The upload may occur while disconnected; hello must resync.
	a.hubMu.Lock()
	connections := []*websocket.Conn{}
	for sub := range a.streams {
		connections = append(connections, sub.conn)
	}
	a.hubMu.Unlock()
	for _, conn := range connections {
		conn.CloseNow()
	}
	put("missed.txt", "recovered")
	until(func() bool { b, _ := os.ReadFile(filepath.Join(folder, "missed.txt")); return string(b) == "recovered" })
	time.Sleep(150 * time.Millisecond)
	before := manifests.Load()
	time.Sleep(300 * time.Millisecond)
	if after := manifests.Load(); before != after {
		t.Fatalf("idle manifest traffic: %d -> %d", before, after)
	}
}
func TestLocalScansDoNotPollServer(t *testing.T) {
	a, _, _, o := testApp(t)
	cert, _, err := ensureTLS(o.TLSDir)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/files" {
			requests.Add(1)
		}
		a.ServeHTTP(w, r)
	}))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	s.StartTLS()
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	folder := t.TempDir()
	go func() {
		done <- runSync(ctx, s.URL, folder, filepath.Join(o.TLSDir, "ca.pem"), false, 100*time.Millisecond)
	}()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for requests.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	before := requests.Load()
	time.Sleep(400 * time.Millisecond)
	if requests.Load() != before {
		t.Fatal("unchanged local scans polled server")
	}
	if err = os.WriteFile(filepath.Join(folder, "local.txt"), []byte("local edit"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		files, err := a.files()
		if err == nil && len(files) == 1 && files[0].Path == "local.txt" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("local change did not upload")
}
