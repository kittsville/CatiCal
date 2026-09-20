package httpserver

import (
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

const defaultPOSTLimit = 10
const defaultPOSTWindow = time.Minute
const defaultICSGETLimit = 30
const defaultICSFeedLimit = 12
const defaultOriginFetchLimit = 16

type ipLimiter struct {
	mu        sync.Mutex
	max       int
	window    time.Duration
	now       func() time.Time
	buckets   map[string][]time.Time
	lastSweep time.Time
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
	return l.allowN(key, 1)
}

func (l *ipLimiter) allowN(key string, n int) bool {
	if n <= 0 {
		return true
	}
	now := l.now()
	cutoff := now.Add(-l.window)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastSweep.IsZero() || now.Sub(l.lastSweep) >= l.window {
		l.sweepLocked(cutoff)
		l.lastSweep = now
	}
	kept := pruneHits(l.buckets[key], cutoff)
	if len(kept)+n > l.max {
		if len(kept) == 0 {
			delete(l.buckets, key)
		} else {
			l.buckets[key] = kept
		}
		return false
	}
	for i := 0; i < n; i++ {
		kept = append(kept, now)
	}
	l.buckets[key] = kept
	return true
}

func (l *ipLimiter) sweepLocked(cutoff time.Time) {
	for k, hits := range l.buckets {
		kept := pruneHits(hits, cutoff)
		if len(kept) == 0 {
			delete(l.buckets, k)
		} else {
			l.buckets[k] = kept
		}
	}
}

func pruneHits(hits []time.Time, cutoff time.Time) []time.Time {
	kept := hits[:0]
	for _, t := range hits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
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

var errOriginRateLimited = errors.New("too many origin fetches")

func serveRobots(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("User-agent: *\nDisallow: /m/\n"))
}
