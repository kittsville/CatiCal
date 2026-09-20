// Package cachepolicy decides whether a cached ICS body is fresh, stale, or unusable.
// Callers pass time.Now; this package does not read the clock.
package cachepolicy

import "time"

const (
	// Fresh is the window where a cached body is served without fetching origins.
	Fresh = 5 * time.Minute
	// MaxStale is the oldest a body may be and still be served after an origin error.
	MaxStale = 24 * time.Hour
)

// Decision is the cache action for a merged (or per-source) body.
type Decision int

const (
	// ServeCache means age < Fresh and a body exists: do not fetch.
	ServeCache Decision = iota
	// FetchServeStaleOnError means Fresh ≤ age ≤ MaxStale: fetch; on error serve the body.
	FetchServeStaleOnError
	// FetchFailOnError means no body or age > MaxStale: fetch; on error fail.
	FetchFailOnError
)

func (d Decision) String() string {
	switch d {
	case ServeCache:
		return "serve_cache"
	case FetchServeStaleOnError:
		return "fetch_serve_stale_on_error"
	case FetchFailOnError:
		return "fetch_fail_on_error"
	default:
		return "unknown"
	}
}

// ShouldFetch reports whether origins must be contacted.
func (d Decision) ShouldFetch() bool {
	return d != ServeCache
}

// ServeCacheOnError reports whether an existing body may be returned if fetch fails.
func (d Decision) ServeCacheOnError() bool {
	return d == ServeCache || d == FetchServeStaleOnError
}

// Decide returns the action for a cache row given mergedAt, now, and whether a body exists.
// Age is now.Sub(mergedAt). Exactly Fresh is stale (fetch); exactly MaxStale may still be served on error.
func Decide(mergedAt, now time.Time, hasBody bool) Decision {
	if !hasBody {
		return FetchFailOnError
	}
	age := now.Sub(mergedAt)
	switch {
	case age < Fresh:
		return ServeCache
	case age <= MaxStale:
		return FetchServeStaleOnError
	default:
		return FetchFailOnError
	}
}
