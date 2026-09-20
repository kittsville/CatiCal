package cachepolicy

import (
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		age     time.Duration
		hasBody bool
		want    Decision
	}{
		{name: "no body ignores age", age: time.Minute, hasBody: false, want: FetchFailOnError},
		{name: "no body even if mergedAt recent", age: 4*time.Minute + 59*time.Second, hasBody: false, want: FetchFailOnError},
		{name: "no body at 5m", age: 5 * time.Minute, hasBody: false, want: FetchFailOnError},
		{name: "no body at 24h1s", age: 24*time.Hour + time.Second, hasBody: false, want: FetchFailOnError},

		{name: "4m59s serve cache", age: 4*time.Minute + 59*time.Second, hasBody: true, want: ServeCache},

		{name: "5m fetch serve stale on error", age: 5 * time.Minute, hasBody: true, want: FetchServeStaleOnError},
		{name: "5m1s fetch serve stale on error", age: 5*time.Minute + time.Second, hasBody: true, want: FetchServeStaleOnError},

		{name: "24h fetch serve stale on error", age: 24 * time.Hour, hasBody: true, want: FetchServeStaleOnError},

		{name: "24h1s fetch fail on error", age: 24*time.Hour + time.Second, hasBody: true, want: FetchFailOnError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mergedAt := now.Add(-tt.age)
			got := Decide(mergedAt, now, tt.hasBody)
			if got != tt.want {
				t.Fatalf("Decide(age=%s, hasBody=%v) = %v, want %v", tt.age, tt.hasBody, got, tt.want)
			}
			switch got {
			case ServeCache:
				if got.ShouldFetch() {
					t.Fatal("ServeCache should not fetch")
				}
				if !got.ServeCacheOnError() {
					t.Fatal("ServeCache must keep serving the body")
				}
			case FetchServeStaleOnError:
				if !got.ShouldFetch() {
					t.Fatal("expected fetch")
				}
				if !got.ServeCacheOnError() {
					t.Fatal("expected stale cache on origin error")
				}
			case FetchFailOnError:
				if !got.ShouldFetch() {
					t.Fatal("expected fetch")
				}
				if got.ServeCacheOnError() {
					t.Fatal("must not serve cache on origin error")
				}
			}
		})
	}
}

func TestWindows(t *testing.T) {
	if Fresh != 5*time.Minute {
		t.Fatalf("Fresh = %s, want 5m", Fresh)
	}
	if MaxStale != 24*time.Hour {
		t.Fatalf("MaxStale = %s, want 24h", MaxStale)
	}
}
