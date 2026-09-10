package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	_ "modernc.org/sqlite"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const schema = `PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS devices(id TEXT PRIMARY KEY,fingerprint TEXT NOT NULL UNIQUE,name TEXT NOT NULL,user_agent TEXT NOT NULL,ip TEXT NOT NULL,first_seen INTEGER NOT NULL,last_seen INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS clips(id INTEGER PRIMARY KEY AUTOINCREMENT,device_id TEXT NOT NULL REFERENCES devices(id),text TEXT NOT NULL,created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS clips_created_at ON clips(created_at DESC);
CREATE TABLE IF NOT EXISTS files(path TEXT PRIMARY KEY COLLATE NOCASE,hash TEXT NOT NULL,size INTEGER NOT NULL,updated INTEGER NOT NULL);`

func openStore(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

type device struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
	Name        string `json:"name"`
	UserAgent   string `json:"userAgent"`
	IP          string `json:"ip"`
	FirstSeen   int64  `json:"firstSeen"`
	LastSeen    int64  `json:"lastSeen"`
}
type clip struct {
	ID        int64  `json:"id"`
	DeviceID  string `json:"deviceId"`
	Text      string `json:"text"`
	CreatedAt int64  `json:"createdAt"`
}
type fileEntry struct {
	Path    string `json:"path"`
	Hash    string `json:"hash"`
	Size    int64  `json:"size"`
	Updated int64  `json:"updated"`
}

func scanDevice(row interface{ Scan(...any) error }) (device, error) {
	var d device
	err := row.Scan(&d.ID, &d.Fingerprint, &d.Name, &d.UserAgent, &d.IP, &d.FirstSeen, &d.LastSeen)
	return d, err
}
func (a *app) identify(w http.ResponseWriter, r *http.Request) (device, error) {
	a.identityMu.Lock()
	defer a.identityMu.Unlock()
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	ua := r.UserAgent()
	h := sha256.Sum256([]byte(ip + "\n" + ua))
	fp := hex.EncodeToString(h[:])[:32]
	d := device{}
	err := sql.ErrNoRows
	if c, e := r.Cookie("coppy_did"); e == nil {
		d, err = scanDevice(a.db.QueryRow("SELECT * FROM devices WHERE id=?", c.Value))
	}
	if err == sql.ErrNoRows {
		d, err = scanDevice(a.db.QueryRow("SELECT * FROM devices WHERE fingerprint=?", fp))
	}
	now := time.Now().UnixMilli()
	if err == sql.ErrNoRows {
		b := make([]byte, 16)
		if _, err = rand.Read(b); err != nil {
			return d, err
		}
		d = device{hex.EncodeToString(b), fp, describeClient(ua, ip), ua, ip, now, now}
		_, err = a.db.Exec("INSERT INTO devices VALUES(?,?,?,?,?,?,?)", d.ID, d.Fingerprint, d.Name, d.UserAgent, d.IP, d.FirstSeen, d.LastSeen)
	} else if err == nil {
		d.IP = ip
		d.UserAgent = ua
		d.LastSeen = now
		_, err = a.db.Exec("UPDATE devices SET ip=?,user_agent=?,last_seen=? WHERE id=?", ip, ua, now, d.ID)
	}
	if err == nil {
		http.SetCookie(w, &http.Cookie{Name: "coppy_did", Value: d.ID, Path: "/", MaxAge: 31536000, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	return d, err
}
func describeClient(ua, ip string) string {
	browser := "Browser"
	for _, v := range []struct{ match, name string }{{"Edg/", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"}, {"Chrome/", "Chrome"}, {"Safari/", "Safari"}} {
		if strings.Contains(ua, v.match) {
			browser = v.name
			break
		}
	}
	system := "Unknown OS"
	for _, v := range []struct{ match, name string }{{"Windows", "Windows"}, {"Android", "Android"}, {"iPhone", "iOS"}, {"iPad", "iOS"}, {"Mac", "macOS"}, {"Linux", "Linux"}} {
		if strings.Contains(ua, v.match) {
			system = v.name
			break
		}
	}
	return fmt.Sprintf("%s on %s (%s)", browser, system, ip)
}
func (a *app) devices() ([]device, error) {
	rows, err := a.db.Query("SELECT * FROM devices ORDER BY first_seen")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []device{}
	for rows.Next() {
		d, e := scanDevice(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func (a *app) clips() ([]clip, error) {
	rows, err := a.db.Query("SELECT id,device_id,text,created_at FROM clips ORDER BY id DESC LIMIT 200")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []clip{}
	for rows.Next() {
		var c clip
		if err = rows.Scan(&c.ID, &c.DeviceID, &c.Text, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}
func (a *app) files() ([]fileEntry, error) {
	rows, err := a.db.Query("SELECT path,hash,size,updated FROM files ORDER BY path")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []fileEntry{}
	for rows.Next() {
		var f fileEntry
		if err = rows.Scan(&f.Path, &f.Hash, &f.Size, &f.Updated); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
