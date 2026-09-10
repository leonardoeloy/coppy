package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestModes(t *testing.T) {
	o, err := parseOptions(nil)
	if err != nil || o.Listen != "127.0.0.1:3737" || o.Background || o.Peer != "" {
		t.Fatal(o, err)
	}
	o, err = parseOptions([]string{"--peer", "192.168.1.20", "."})
	cwd, _ := os.Getwd()
	if err != nil || o.Dir != cwd || o.Background {
		t.Fatal(o, err)
	}
	o, err = parseOptions([]string{"--peer", "192.168.1.20", "-b", "."})
	if err != nil || !o.Background {
		t.Fatal(o, err)
	}
	for _, args := range [][]string{{"--peer", ""}, {"--peer", "localhost", "--dir", "x", "."}, {"--once"}, {"--peer", "localhost", "-b", "--once"}, {"."}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}
func TestEmbeddedCertificateAndProcesses(t *testing.T) {
	a, s, _, o := testApp(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "coppy")
	build := exec.Command("go", "build", "-ldflags", "-X main.embeddedCA="+base64.StdEncoding.EncodeToString(a.ca), "-o", bin, ".")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, b)
	}
	current := filepath.Join(dir, "shared")
	os.Mkdir(current, 0700)
	os.WriteFile(filepath.Join(current, "from-current.txt"), []byte("embedded trust"), 0600)
	cmd := exec.Command(bin, "--peer", strings.TrimPrefix(s.URL, "https://"), "--once", ".")
	cmd.Dir = current
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("embedded CA sync: %v %s", err, b)
	}
	files, err := a.files()
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	// No -b means a long-running foreground process that releases its lock on interrupt.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	foreground := exec.CommandContext(ctx, bin, "--peer", s.URL, ".")
	foreground.Dir = current
	if err = foreground.Start(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(current, ".coppy-lock"))
	if err = foreground.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err = foreground.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(current, ".coppy-lock")); !os.IsNotExist(err) {
		t.Fatal("foreground lock leaked")
	}
	background := exec.Command(bin, "--peer", s.URL, "-b", ".")
	background.Dir = current
	b, err := background.CombinedOutput()
	if err != nil {
		t.Fatalf("background: %v %s", err, b)
	}
	stopBackground(t, b, filepath.Join(current, ".coppy-lock"))
	// The same executable starts a server without --peer, reusing the supplied identity.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	data := filepath.Join(dir, "server")
	server := exec.Command(bin, "--listen", address, "--data", data, "--tls-dir", o.TLSDir, "-b")
	output, err := server.CombinedOutput()
	if err != nil {
		t.Fatalf("server background: %v %s", err, output)
	}
	defer stopBackground(t, output, filepath.Join(data, "coppy.db.lock"))
	client, err := secureClient(filepath.Join(o.TLSDir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Get("https://" + address + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Shared files") {
		t.Fatal("standalone web UI missing")
	}
	// The launcher must report a startup failure, not success, when the port is taken.
	duplicate := exec.Command(bin, "--listen", address, "--data", filepath.Join(dir, "duplicate"), "--tls-dir", o.TLSDir, "-b")
	if out, err := duplicate.CombinedOutput(); err == nil {
		t.Fatalf("accepted occupied port: %s", out)
	}
}
func waitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("not created:", path)
}
func stopBackground(t *testing.T, output []byte, lock string) {
	t.Helper()
	match := regexp.MustCompile(`PID (\d+)`).FindSubmatch(output)
	if len(match) != 2 {
		t.Fatal(string(output))
	}
	pid, _ := strconv.Atoi(string(match[1]))
	p, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err = os.Stat(lock); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.Kill()
	t.Fatal(fmt.Sprintf("background %d failed to stop", pid))
}
