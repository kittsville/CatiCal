package httpserver

import (
	"testing"
	"time"
)

func TestIPLimiterAllowNRejectsWithoutPartialConsume(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	l := newIPLimiter(3, time.Minute, func() time.Time { return now })
	if !l.allowN("a", 2) {
		t.Fatal("first burst should fit")
	}
	if l.allowN("a", 2) {
		t.Fatal("second burst must not partially consume")
	}
	if !l.allowN("a", 1) {
		t.Fatal("remaining slot should still be available")
	}
}

func TestIPLimiterSweepsIdleKeys(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	l := newIPLimiter(2, time.Minute, func() time.Time { return now })
	if !l.allow("old") {
		t.Fatal("seed")
	}
	now = now.Add(2 * time.Minute)
	if !l.allow("new") {
		t.Fatal("new key")
	}
	l.mu.Lock()
	_, old := l.buckets["old"]
	n := len(l.buckets)
	l.mu.Unlock()
	if old {
		t.Fatal("idle key should be evicted after window")
	}
	if n != 1 {
		t.Fatalf("buckets = %d, want 1", n)
	}
}
