package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func buildDownloads(o options) error {
	_, ca, err := ensureTLS(o.TLSDir)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(o.Downloads, 0755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(o.Downloads, ".coppy-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for _, system := range []string{"windows", "darwin"} {
		for _, arch := range []string{"amd64", "arm64"} {
			name := "coppy-" + system + "-" + arch
			if system == "windows" {
				name += ".exe"
			}
			fmt.Println("Building", name)
			cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w -X main.embeddedCA="+base64.StdEncoding.EncodeToString(ca), "-o", filepath.Join(stage, name), ".")
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+system, "GOARCH="+arch)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err = cmd.Run(); err != nil {
				return err
			}
		}
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err = os.Rename(filepath.Join(stage, e.Name()), filepath.Join(o.Downloads, e.Name())); err != nil {
			return err
		}
	}
	b, _ := json.Marshal(map[string]string{"fingerprint": fingerprint(ca)})
	if err = os.WriteFile(filepath.Join(o.Downloads, ".coppy-ca.json.tmp"), b, 0644); err != nil {
		return err
	}
	return os.Rename(filepath.Join(o.Downloads, ".coppy-ca.json.tmp"), filepath.Join(o.Downloads, ".coppy-ca.json"))
}
