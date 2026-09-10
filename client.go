package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type entry struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}
type syncer struct {
	base, dir string
	client    *http.Client
	baseline  map[string]string
}

func runSync(ctx context.Context, base, dir, ca string, once bool, interval time.Duration) error {
	client, err := secureClient(ca)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock := filepath.Join(dir, ".coppy-lock")
	if err = os.Mkdir(lock, 0700); err != nil {
		return fmt.Errorf("folder is locked; if no client is running, remove %s", lock)
	}
	defer os.Remove(lock)
	client.Transport = contextTransport{ctx, client.Transport}
	s := syncer{base, dir, client, map[string]string{}}
	statePath := filepath.Join(dir, ".coppy-state.json")
	if b, e := os.ReadFile(statePath); e == nil {
		if e = json.Unmarshal(b, &s.baseline); e != nil {
			return e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	probe, err := client.Get(base + "/api/files")
	if err != nil {
		return err
	}
	probe.Body.Close()
	if probe.StatusCode != 200 {
		return fmt.Errorf("server: %s", probe.Status)
	}
	if err = markReady(); err != nil {
		return err
	}
	for {
		err = s.run()
		if err == nil {
			var b []byte
			b, err = json.Marshal(s.baseline)
			if err == nil {
				err = os.WriteFile(statePath+".tmp", b, 0600)
			}
			if err == nil {
				err = os.Rename(statePath+".tmp", statePath)
			}
		}
		if once {
			return err
		}
		if err != nil {
			log.Print(err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
func digest(p string) (string, error) {
	f, e := os.Open(p)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	_, e = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), e
}
func (s *syncer) endpoint(p string) string { return s.base + "/api/file?path=" + url.QueryEscape(p) }
func (s *syncer) run() error {
	r, e := s.client.Get(s.base + "/api/files")
	if e != nil {
		return e
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return fmt.Errorf("list: %s", r.Status)
	}
	var manifest struct {
		Files []entry `json:"files"`
	}
	if e = json.NewDecoder(r.Body).Decode(&manifest); e != nil {
		return e
	}
	remote := map[string]string{}
	for _, f := range manifest.Files {
		if !safe(f.Path) {
			return fmt.Errorf("unsafe remote path %q", f.Path)
		}
		remote[f.Path] = f.Hash
	}
	local := map[string]string{}
	e = filepath.WalkDir(s.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == s.dir {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".coppy-") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(s.dir, p)
		rel = filepath.ToSlash(rel)
		if !safe(rel) {
			return fmt.Errorf("non-portable path %q", rel)
		}
		h, err := digest(p)
		if err == nil {
			local[rel] = h
		}
		return err
	})
	if e != nil {
		return e
	}
	for p, h := range local {
		rh := remote[p]
		if h == rh {
			s.baseline[p] = h
			continue
		}
		old := s.baseline[p]
		if rh != "" && h == old {
			continue
		}
		target := p
		if rh != "" && rh != old {
			ext := filepath.Ext(p)
			target = strings.TrimSuffix(p, ext) + ".conflict-" + h[:12] + ext
			log.Printf("Concurrent edit: preserving %s as %s", p, target)
		}
		f, err := os.Open(filepath.Join(s.dir, filepath.FromSlash(p)))
		if err != nil {
			return err
		}
		req, err := http.NewRequest("PUT", s.endpoint(target), f)
		if err != nil {
			f.Close()
			return err
		}
		req.Header.Set("If-Match", remote[target])
		resp, err := s.client.Do(req)
		f.Close()
		if err != nil {
			return err
		}
		var result struct {
			File entry `json:"file"`
		}
		decErr := json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("upload %s: %s", target, resp.Status)
		}
		if decErr != nil {
			return decErr
		}
		if result.File.Hash != h {
			return fmt.Errorf("%s changed during upload; retrying", p)
		}
		if target == p {
			remote[p] = h
			s.baseline[p] = h
		} else {
			// Preserve local bytes before replacing the original with the remote version.
			dest := filepath.Join(s.dir, filepath.FromSlash(target))
			if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("conflict destination exists: %s", dest)
			}
			current, err := digest(filepath.Join(s.dir, filepath.FromSlash(p)))
			if err != nil || current != h {
				return fmt.Errorf("local file changed: %s", p)
			}
			if err = os.Rename(filepath.Join(s.dir, filepath.FromSlash(p)), dest); err != nil {
				return err
			}
			delete(local, p)
			s.baseline[target] = h
		}
	}
	for p, h := range remote {
		if local[p] == h {
			continue
		}
		if e = s.download(p, h, local[p]); e != nil {
			return e
		}
		s.baseline[p] = h
	}
	return nil
}
func safe(p string) bool {
	if p == "" || len(p) > 1024 {
		return false
	}
	for _, v := range strings.Split(p, "/") {
		if v == "" || v == "." || v == ".." || strings.HasPrefix(v, ".coppy-") || strings.ContainsAny(v, "\\<>:\"|?*\x00") || strings.HasSuffix(v, ".") || strings.HasSuffix(v, " ") {
			return false
		}
		for _, c := range v {
			if c < 32 {
				return false
			}
		}
		stem := strings.ToUpper(strings.Split(v, ".")[0])
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || (len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '0' && stem[3] <= '9') {
			return false
		}
	}
	return true
}
func (s *syncer) download(p, h, old string) error {
	dest := filepath.Join(s.dir, filepath.FromSlash(p))
	// Refuse symlinks in every existing component, including the destination.
	cur := s.dir
	for _, part := range strings.Split(p, "/") {
		cur = filepath.Join(cur, part)
		st, e := os.Lstat(cur)
		if e == nil && st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink: %s", cur)
		}
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	if e := os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
		return e
	}
	r, e := s.client.Get(s.endpoint(p))
	if e != nil {
		return e
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return fmt.Errorf("download: %s", r.Status)
	}
	tmp, e := os.CreateTemp(filepath.Dir(dest), ".coppy-download-")
	if e != nil {
		return e
	}
	defer os.Remove(tmp.Name())
	hash := sha256.New()
	_, e = io.Copy(io.MultiWriter(tmp, hash), r.Body)
	closeErr := tmp.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if hex.EncodeToString(hash.Sum(nil)) != h {
		return fmt.Errorf("remote changed: %s", p)
	}
	current, e := digest(dest)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if current != old {
		return fmt.Errorf("local changed: %s", p)
	}
	return os.Rename(tmp.Name(), dest)
}

type contextTransport struct {
	ctx   context.Context
	inner http.RoundTripper
}

func (t contextTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.inner.RoundTrip(r.WithContext(t.ctx))
}

func (t contextTransport) CloseIdleConnections() {
	if c, ok := t.inner.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}
