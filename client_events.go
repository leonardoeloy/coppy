package main

import (
	"context"
	"encoding/json"
	"github.com/coder/websocket"
	"log"
	"net/http"
	"strings"
	"time"
)

// The HTTPS client preserves embedded/explicit CA verification for WSS too.
// Reconnection's hello forces a full resync to recover events missed offline.
func watchFileEvents(ctx context.Context, base string, client *http.Client, changes chan<- struct{}) {
	delay := time.Second
	for ctx.Err() == nil {
		conn, _, err := websocket.Dial(ctx, "wss://"+strings.TrimPrefix(base, "https://")+"/api/ws", &websocket.DialOptions{HTTPClient: client})
		if err == nil {
			delay = time.Second
			conn.SetReadLimit(2 << 20)
			for {
				_, data, e := conn.Read(ctx)
				if e != nil {
					err = e
					break
				}
				var message struct {
					Event string `json:"event"`
				}
				if e = json.Unmarshal(data, &message); e != nil {
					err = e
					break
				}
				if message.Event == "hello" || message.Event == "file" {
					select {
					case changes <- struct{}{}:
					default:
					}
				}
			}
			conn.CloseNow()
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("WebSocket disconnected; retrying in %s: %v", delay, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}
