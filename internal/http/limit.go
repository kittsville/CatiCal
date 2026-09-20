package httpserver

import (
	"net"
	"net/http"
	"sync"
	"time"
)

const defaultPOSTLimit = 10
const defaultPOSTWindow = time.Minute

type ipLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	now     func() time.Time
	buckets map[string][]time.Time
}

func newIPLimiter(max int, window time.Duration, now func() time.Time) *ipLimiter {
	if max <= 0 {
		max = defaultPOSTLimit
	}
	if window <= 0 {
		window = defaultPOSTWindow
	}
	if now == nil {
		now = time.Now
	}
	return &ipLimiter{
		max:     max,
		window:  window,
		now:     now,
		buckets: make(map[string][]time.Time),
	}
}

func (l *ipLimiter) allow(key string) bool {
	now := l.now()
	cutoff := now.Add(-l.window)
	l.mu.Lock()
	defer l.mu.Unlock()
	hits := l.buckets[key]
	kept := hits[:0]
	for _, t := range hits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.buckets[key] = kept
		return false
	}
	l.buckets[key] = append(kept, now)
	return true
}

func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		if r.RemoteAddr == "" {
			return "unknown"
		}
		return r.RemoteAddr
	}
	return host
}

func tooMany(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Retry-After", "60")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte("too many requests\n"))
}

func serveRobots(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("User-agent: *\nDisallow: /m/\n"))
}
