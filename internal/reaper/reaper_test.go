package reaper

import (
	"bytes"
	"context"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	httpserver "sci1.uk/catical/internal/http"
	"sci1.uk/catical/internal/store"
	"sci1.uk/catical/internal/tokens"
)

func TestRunOnceDeletes15DayFeedKeeps13Day(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	staleID := uuid.New()
	freshID := uuid.New()
	st := &memDeleter{feeds: map[uuid.UUID]store.Feed{
		staleID: {ID: staleID, LastRequestAt: now.Add(-15 * 24 * time.Hour)},
		freshID: {ID: freshID, LastRequestAt: now.Add(-13 * 24 * time.Hour)},
	}}
	w := Worker{
		Store:     st,
		Now:       func() time.Time { return now },
		Retention: DefaultRetention,
		Logger:    slog.New(slog.NewTextHandler(new(bytes.Buffer), nil)),
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.feeds[staleID]; ok {
		t.Fatal("15-day-old feed should be deleted")
	}
	if _, ok := st.feeds[freshID]; !ok {
		t.Fatal("13-day-old feed should be kept")
	}
	if st.deleteCalls != 1 {
		t.Fatalf("DeleteStaleFeeds calls = %d, want 1", st.deleteCalls)
	}
	if !st.lastNow.Equal(now) {
		t.Fatalf("passed now %v, want %v", st.lastNow, now)
	}
	if st.lastRetention != DefaultRetention {
		t.Fatalf("retention %v, want %v", st.lastRetention, DefaultRetention)
	}
}

func TestRunOnceThenFeedGETIs404(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	staleSalt, staleSecret, staleHash, err := tokens.Generate()
	if err != nil {
		t.Fatal(err)
	}
	freshSalt, freshSecret, freshHash, err := tokens.Generate()
	if err != nil {
		t.Fatal(err)
	}
	staleID := uuid.New()
	freshID := uuid.New()
	st := &memDeleter{feeds: map[uuid.UUID]store.Feed{
		staleID: {
			ID:            staleID,
			FeedTokenSalt: staleSalt,
			FeedTokenHash: staleHash,
			LastRequestAt: now.Add(-15 * 24 * time.Hour),
		},
		freshID: {
			ID:            freshID,
			FeedTokenSalt: freshSalt,
			FeedTokenHash: freshHash,
			LastRequestAt: now.Add(-13 * 24 * time.Hour),
		},
	}}
	w := Worker{
		Store:     st,
		Now:       func() time.Time { return now },
		Retention: DefaultRetention,
		Logger:    slog.New(slog.NewTextHandler(ioDiscard{}, nil)),
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	h := httpserver.New(httpserver.Config{
		Store:   st,
		Refresh: staticRefresh{body: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")},
		Now:     func() time.Time { return now },
	})
	stalePath := "/c/" + staleID.String() + "/" + hex.EncodeToString(staleSecret) + ".ics"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, stalePath, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("stale feed GET status = %d, want 404", rec.Code)
	}
	freshPath := "/c/" + freshID.String() + "/" + hex.EncodeToString(freshSecret) + ".ics"
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, freshPath, nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("kept feed GET status = %d, want 200", rec2.Code)
	}
}

func TestRunOnceLogsCountNotIDs(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	id := uuid.New()
	st := &memDeleter{feeds: map[uuid.UUID]store.Feed{
		id: {ID: id, LastRequestAt: now.Add(-15 * 24 * time.Hour)},
	}}
	var buf bytes.Buffer
	w := Worker{
		Store:     st,
		Now:       func() time.Time { return now },
		Retention: DefaultRetention,
		Logger:    slog.New(slog.NewTextHandler(&buf, nil)),
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "deleted") || !strings.Contains(logged, "1") {
		t.Fatalf("expected deleted count in log: %s", logged)
	}
	if strings.Contains(logged, id.String()) {
		t.Fatalf("log leaked feed id: %s", logged)
	}
}

func TestLoopUsesInjectedNowAndStops(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	st := &memDeleter{feeds: map[uuid.UUID]store.Feed{}}
	ctx, cancel := context.WithCancel(context.Background())
	w := Worker{
		Store:     st,
		Now:       func() time.Time { return now },
		Every:     20 * time.Millisecond,
		Retention: DefaultRetention,
		Logger:    slog.New(slog.NewTextHandler(ioDiscard{}, nil)),
	}
	done := make(chan struct{})
	go func() {
		w.Loop(ctx)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st.mu.Lock()
		n := st.deleteCalls
		st.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Loop did not stop")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.deleteCalls < 1 {
		t.Fatal("Loop never called DeleteStaleFeeds")
	}
	if !st.lastNow.Equal(now) {
		t.Fatalf("loop now = %v, want frozen %v", st.lastNow, now)
	}
}

type memDeleter struct {
	mu            sync.Mutex
	feeds         map[uuid.UUID]store.Feed
	deleteCalls   int
	lastNow       time.Time
	lastRetention time.Duration
}

func (m *memDeleter) GetFeed(_ context.Context, id uuid.UUID) (store.Feed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.feeds[id]
	if !ok {
		return store.Feed{}, store.ErrNotFound
	}
	return f, nil
}

func (m *memDeleter) TouchLastRequest(_ context.Context, id uuid.UUID, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.feeds[id]
	if !ok {
		return store.ErrNotFound
	}
	f.LastRequestAt = at
	m.feeds[id] = f
	return nil
}

func (m *memDeleter) DeleteStaleFeeds(_ context.Context, now time.Time, retention time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCalls++
	m.lastNow = now
	m.lastRetention = retention
	cutoff := now.Add(-retention)
	var n int64
	for id, f := range m.feeds {
		if f.LastRequestAt.Before(cutoff) {
			delete(m.feeds, id)
			n++
		}
	}
	return n, nil
}

type staticRefresh struct{ body []byte }

func (s staticRefresh) Refresh(context.Context, uuid.UUID) ([]byte, error) {
	return s.body, nil
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
