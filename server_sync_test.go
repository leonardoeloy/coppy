package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServerAlsoSyncsFolder(t *testing.T) {
	folder := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	o, err := parseOptions([]string{"--dir", folder, "--listen", address, "--interval", "1s", "--data", filepath.Join(folder, "internal"), "--files", filepath.Join(folder, "blobs"), "--db", filepath.Join(folder, "index.db"), "--tls-dir", filepath.Join(folder, "certificates"), "--downloads", filepath.Join(folder, "downloads")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = ensureTLS(o.TLSDir); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(folder, "mac.txt"), []byte("from Mac"), 0600); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(o.Downloads, 0700)
	os.WriteFile(filepath.Join(o.Downloads, "binary"), []byte("internal executable"), 0600)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, o) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("server did not stop")
		}
		for _, lock := range []string{o.DB + ".lock", filepath.Join(folder, ".coppy-lock")} {
			if _, err := os.Stat(lock); !os.IsNotExist(err) {
				t.Error("lock leaked", lock)
			}
		}
	}()
	client, err := secureClient(filepath.Join(o.TLSDir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	client.Timeout = 2 * time.Second
	base := "https://" + address
	manifest := func() []fileEntry {
		response, err := client.Get(base + "/api/files")
		if err != nil {
			return nil
		}
		defer response.Body.Close()
		var data struct{ Files []fileEntry }
		json.NewDecoder(response.Body).Decode(&data)
		return data.Files
	}
	waitUntil := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("sync did not converge")
	}
	waitUntil(func() bool { files := manifest(); return len(files) == 1 && files[0].Path == "mac.txt" })
	peer := t.TempDir()
	syncPeer := func() {
		t.Helper()
		if err := runSync(context.Background(), base, peer, filepath.Join(o.TLSDir, "ca.pem"), true, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	syncPeer()
	b, err := os.ReadFile(filepath.Join(peer, "mac.txt"))
	if err != nil || string(b) != "from Mac" {
		t.Fatal(string(b), err)
	}
	os.WriteFile(filepath.Join(peer, "windows.txt"), []byte("from Windows"), 0600)
	syncPeer()
	waitUntil(func() bool {
		b, err := os.ReadFile(filepath.Join(folder, "windows.txt"))
		return err == nil && string(b) == "from Windows"
	})
	os.WriteFile(filepath.Join(folder, "mac.txt"), []byte("edited on server"), 0600)
	expected, _ := digest(filepath.Join(folder, "mac.txt"))
	waitUntil(func() bool {
		for _, f := range manifest() {
			if f.Path == "mac.txt" && f.Hash == expected {
				return true
			}
		}
		return false
	})
	syncPeer()
	b, err = os.ReadFile(filepath.Join(peer, "mac.txt"))
	if err != nil || string(b) != "edited on server" {
		t.Fatal(string(b), err)
	}
	// A peer cannot overwrite the server's private key or database via a manifest path.
	keyPath := filepath.Join(o.TLSDir, "server-key.pem")
	before, _ := os.ReadFile(keyPath)
	empty := ""
	status, _, _ := request(t, client, "PUT", base+"/api/file?path=certificates/server-key.pem", []byte("not a key"), &empty)
	if status != 200 {
		t.Fatal(status)
	}
	status, _, _ = request(t, client, "PUT", base+"/api/file?path=index.db", []byte("not a database"), &empty)
	if status != 200 {
		t.Fatal(status)
	}
	status, _, _ = request(t, client, "PUT", base+"/api/file?path=arrived.txt", []byte("browser upload"), &empty)
	if status != 200 {
		t.Fatal(status)
	}
	waitUntil(func() bool {
		b, err := os.ReadFile(filepath.Join(folder, "arrived.txt"))
		return err == nil && string(b) == "browser upload"
	})
	after, _ := os.ReadFile(keyPath)
	if string(before) != string(after) {
		t.Fatal("private key overwritten")
	}
	for _, f := range manifest() {
		if f.Path != "mac.txt" && f.Path != "windows.txt" && f.Path != "certificates/server-key.pem" && f.Path != "index.db" && f.Path != "arrived.txt" {
			t.Fatal("internal file leaked", f.Path)
		}
	}
}
func TestServerSyncRejectsStorageRoot(t *testing.T) {
	root := t.TempDir()
	o, err := parseOptions([]string{"--dir", root, "--data", root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = serverExclusions(o); err == nil {
		t.Fatal("accepted storage root as sync folder")
	}
}
