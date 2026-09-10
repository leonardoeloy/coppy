package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

type options struct {
	Peer, Dir, CA, Listen, Data, DB, Files, TLSDir, Downloads string
	Background, Once, Build                                   bool
	Interval                                                  time.Duration
}

func parseOptions(args []string) (options, error) {
	home, _ := os.UserHomeDir()
	o := options{}
	f := flag.NewFlagSet("coppy", flag.ContinueOnError)
	f.StringVar(&o.Peer, "peer", "", "HTTPS peer; omitted serves and syncs a local folder")
	f.StringVar(&o.Dir, "dir", filepath.Join(home, "Coppy"), "Folder to sync: server defaults to current directory, client to ~/Coppy")
	f.StringVar(&o.CA, "ca", "", "Override embedded public CA with a PEM file")
	f.BoolVar(&o.Background, "b", false, "Run in background")
	f.BoolVar(&o.Once, "once", false, "Sync once and exit")
	f.DurationVar(&o.Interval, "interval", 3*time.Second, "Local folder scan/retry interval")
	f.StringVar(&o.Listen, "listen", "127.0.0.1:3737", "Server bind address; use 0.0.0.0:3737 for LAN")
	f.StringVar(&o.Data, "data", ".coppy-server", "Server data directory")
	f.StringVar(&o.DB, "db", os.Getenv("COPPY_DB"), "SQLite path (defaults inside --data)")
	f.StringVar(&o.Files, "files", os.Getenv("COPPY_FILES"), "Blob directory (defaults inside --data)")
	f.StringVar(&o.TLSDir, "tls-dir", os.Getenv("COPPY_TLS_DIR"), "Certificate directory (defaults inside --data)")
	f.StringVar(&o.Downloads, "downloads", "dist", "Directory of downloadable binaries")
	f.BoolVar(&o.Build, "build-downloads", false, "Build four binaries embedding this server's public CA (requires Go sources)")
	if err := f.Parse(args); err != nil {
		return o, err
	}
	set := map[string]bool{}
	f.Visit(func(v *flag.Flag) { set[v.Name] = true })
	if o.Interval < time.Second || f.NArg() > 1 || (f.NArg() > 0 && set["dir"]) || (set["peer"] && o.Peer == "") {
		return o, fmt.Errorf("invalid options; use coppy [-b] [folder] or coppy --peer IP [-b] [folder]")
	}
	if o.Peer == "" {
		if o.Once || set["ca"] {
			return o, fmt.Errorf("--ca and --once require --peer")
		}
		if !set["dir"] {
			o.Dir = "."
		}
	} else {
		if o.Build {
			return o, fmt.Errorf("--build-downloads cannot be combined with --peer")
		}
		var err error
		o.Peer, err = peerURL(o.Peer)
		if err != nil {
			return o, err
		}
	}
	if f.NArg() == 1 {
		o.Dir = f.Arg(0)
	}
	if o.Build && o.Background {
		return o, fmt.Errorf("builds run in the foreground")
	}
	if o.Once && o.Background {
		return o, fmt.Errorf("--once cannot be combined with -b")
	}
	for _, p := range []*string{&o.Dir, &o.CA, &o.Data, &o.DB, &o.Files, &o.TLSDir, &o.Downloads} {
		if *p != "" {
			abs, err := filepath.Abs(*p)
			if err != nil {
				return o, err
			}
			*p = abs
		}
	}
	if o.DB == "" {
		o.DB = filepath.Join(o.Data, "coppy.db")
	}
	if o.Files == "" {
		o.Files = filepath.Join(o.Data, "files")
	}
	if o.TLSDir == "" {
		o.TLSDir = filepath.Join(o.Data, "tls")
	}
	return o, nil
}
func main() {
	if err := runMain(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func runMain() error {
	o, err := parseOptions(os.Args[1:])
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if o.Build {
		return buildDownloads(o)
	}
	if o.Background {
		return launchBackground(o)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if o.Peer != "" {
		log.Printf("Syncing %s with %s (foreground; use -b for background)", o.Dir, o.Peer)
		err = runSync(ctx, o.Peer, o.Dir, o.CA, o.Once, o.Interval)
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	return runServer(ctx, o)
}
func launchBackground(o options) error {
	dir := o.Data
	args := []string{"--dir", o.Dir, "--interval", o.Interval.String(), "--listen", o.Listen, "--data", o.Data, "--db", o.DB, "--files", o.Files, "--tls-dir", o.TLSDir, "--downloads", o.Downloads}
	if o.Peer != "" {
		dir = o.Dir
		args = []string{"--peer", o.Peer, "--dir", o.Dir, "--ca", o.CA, "--interval", o.Interval.String()}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	ready, err := os.CreateTemp(dir, ".coppy-ready-")
	if err != nil {
		return err
	}
	name := ready.Name()
	ready.Close()
	os.Remove(name)
	defer os.Remove(name)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := filepath.Join(dir, ".coppy.log")
	out, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "COPPY_READY_FILE="+name)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = detach()
	if err = cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return fmt.Errorf("background startup failed (%v); see %s", err, logPath)
		case <-deadline.C:
			cmd.Process.Kill()
			<-done
			return fmt.Errorf("background startup timed out; see %s", logPath)
		case <-ticker.C:
			if _, err := os.Stat(name); err == nil {
				fmt.Printf("Coppy running in background (PID %d). Log: %s\n", cmd.Process.Pid, logPath)
				return nil
			}
		}
	}
}
func markReady() error {
	if p := os.Getenv("COPPY_READY_FILE"); p != "" {
		return os.WriteFile(p, []byte("ready"), 0600)
	}
	return nil
}
