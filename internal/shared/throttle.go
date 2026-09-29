package shared

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// throttle counts attempts per key in a sliding window (login and friends:
// nothing used to slow down password guessing — review, 2026-09-28). In
// memory, one server; a restart forgets, which is fine for this purpose.
type throttle struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
	swept  time.Time
}

func newThrottle(max int, window time.Duration) *throttle {
	return &throttle{max: max, window: window, hits: map[string][]time.Time{}}
}

// allow records an attempt for key and reports whether it is within the limit.
func (t *throttle) allow(key string) bool {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if now.Sub(t.swept) > t.window { // drop idle keys so random names cannot grow the map for ever
		for k, v := range t.hits {
			if len(v) == 0 || now.Sub(v[len(v)-1]) > t.window {
				delete(t.hits, k)
			}
		}
		t.swept = now
	}
	kept := t.hits[key][:0]
	for _, at := range t.hits[key] {
		if now.Sub(at) < t.window {
			kept = append(kept, at)
		}
	}
	if len(kept) >= t.max {
		t.hits[key] = kept
		return false
	}
	t.hits[key] = append(kept, now)
	return true
}

var (
	ipAttempts      = newThrottle(20, 5*time.Minute)  // login / forgot / set-password, per client address
	accountAttempts = newThrottle(10, 15*time.Minute) // logins per account (not the shared anonymous one)
)

// clientIP is the address Apache saw: the LAST X-Forwarded-For entry (the
// one it appended — earlier entries are whatever the client claimed),
// else the connection's own address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// tooMany answers 429 when the client address is over its limit.
func tooMany(w http.ResponseWriter, r *http.Request) bool {
	if ipAttempts.allow(clientIP(r)) {
		return false
	}
	http.Error(w, "too many attempts — try again in a few minutes", http.StatusTooManyRequests)
	return true
}
