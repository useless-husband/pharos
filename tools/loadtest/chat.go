package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sync"
	"time"
)

// chat is a local stand-in for a Discord webhook. It enforces Discord's
// webhook rate limits (5 messages per 2 seconds, and 30 per minute per
// channel) and answers excess requests with 429 and a Retry-After, as
// Discord does.
type chat struct {
	mu       sync.Mutex
	accepted []time.Time
	limited  int
}

func startChat() (*chat, string) {
	c := &chat{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	go http.Serve(ln, c) //nolint:errcheck
	return c, "http://" + ln.Addr().String() + "/api/webhooks/1/token"
}

func (c *chat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := c.wait(now); wait > 0 {
		c.limited++
		w.Header().Set("Retry-After", fmt.Sprint(math.Ceil(wait.Seconds())))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "You are being rate limited.", "retry_after": wait.Seconds(), "global": false})
		return
	}
	c.accepted = append(c.accepted, now)
	w.WriteHeader(http.StatusNoContent)
}

// wait is how long until another message is allowed. Caller holds c.mu.
func (c *chat) wait(now time.Time) time.Duration {
	var d time.Duration
	for _, lim := range []struct {
		n      int
		window time.Duration
	}{{5, 2 * time.Second}, {30, time.Minute}} {
		if len(c.accepted) >= lim.n {
			if free := c.accepted[len(c.accepted)-lim.n].Add(lim.window); free.After(now) {
				d = max(d, free.Sub(now))
			}
		}
	}
	return d
}

func (c *chat) report() (posted, limited int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.accepted), c.limited
}
