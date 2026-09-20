package httpserver

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"sci1.uk/catical/internal/refresh"
	"sci1.uk/catical/internal/store"
	"sci1.uk/catical/internal/tokens"
)

func TestFeedGETValidSecret(t *testing.T) {
	id := uuid.New()
	salt, secret, hash, err := tokens.Generate()
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n")
	st := &fakeFeedStore{feed: store.Feed{
		ID:            id,
		FeedTokenSalt: salt,
		FeedTokenHash: hash,
	}}
	rf := &fakeRefresh{body: body}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	h := New(Config{Store: st, Refresh: rf, Now: func() time.Time { return now }})
	req := httptest.NewRequest(http.MethodGet, feedPath(id, secret)+".ics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "text/calendar; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "private, max-age=300" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	got := rec.Body.Bytes()
	if !bytes.HasPrefix(got, []byte("BEGIN:VCALENDAR")) {
		t.Fatalf("body start: %q", got)
	}
	if !bytes.Contains(got, []byte("\r\n")) {
		t.Fatal("expected CRLF")
	}
	if !st.touched {
		t.Fatal("expected TouchLastRequest")
	}
	if !st.touchedAt.Equal(now) {
		t.Fatalf("touch at %v, want %v", st.touchedAt, now)
	}
	if rf.calls != 1 {
		t.Fatalf("refresh calls = %d", rf.calls)
	}
}

func TestFeedGETAcceptsSecretWithoutICSSuffix(t *testing.T) {
	id := uuid.New()
	salt, secret, hash, err := tokens.Generate()
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeFeedStore{feed: store.Feed{ID: id, FeedTokenSalt: salt, FeedTokenHash: hash}}
	rf := &fakeRefresh{body: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")}
	h := New(Config{Store: st, Refresh: rf})

	req := httptest.NewRequest(http.MethodGet, feedPath(id, secret), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestFeedGETWrongSecretAndUnknownIDAre404(t *testing.T) {
	id := uuid.New()
	salt, secret, hash, err := tokens.Generate()
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeFeedStore{feed: store.Feed{ID: id, FeedTokenSalt: salt, FeedTokenHash: hash}}
	rf := &fakeRefresh{body: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")}
	h := New(Config{Store: st, Refresh: rf})

	_, other, _, err := tokens.Generate()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		url  string
	}{
		{"wrong secret", feedPath(id, other) + ".ics"},
		{"unknown id", feedPath(uuid.New(), secret) + ".ics"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if rec.Body.String() == "BEGIN:VCALENDAR" {
				t.Fatal("must not leak calendar body")
			}
		})
	}
	if rf.calls != 0 {
		t.Fatalf("refresh must not run on auth failure, calls=%d", rf.calls)
	}
	if st.touched {
		t.Fatal("must not touch last_request on 404")
	}
}

func TestFeedGETInvalidUUIDIs404(t *testing.T) {
	h := New(Config{Store: &fakeFeedStore{}, Refresh: &fakeRefresh{}})
	req := httptest.NewRequest(http.MethodGet, "/c/not-a-uuid/deadbeef.ics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestFeedGETRefreshFailIs502PlainText(t *testing.T) {
	id := uuid.New()
	salt, secret, hash, err := tokens.Generate()
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeFeedStore{feed: store.Feed{ID: id, FeedTokenSalt: salt, FeedTokenHash: hash}}
	rf := &fakeRefresh{err: refresh.ErrNoUsableSources}
	h := New(Config{Store: st, Refresh: rf})

	req := httptest.NewRequest(http.MethodGet, feedPath(id, secret)+".ics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "<html") {
		t.Fatal("must not return HTML")
	}
	if st.touched {
		t.Fatal("unsuccessful GET must not touch last_request")
	}
}

func TestFeedGETLogsRedactSecret(t *testing.T) {
	id := uuid.New()
	salt, secret, hash, err := tokens.Generate()
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeFeedStore{feed: store.Feed{ID: id, FeedTokenSalt: salt, FeedTokenHash: hash}}
	rf := &fakeRefresh{body: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")}
	buf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(buf, nil))
	h := New(Config{Store: st, Refresh: rf, Logger: log})

	path := feedPath(id, secret) + ".ics"
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	logged := buf.String()
	hexSecret := hex.EncodeToString(secret)
	if strings.Contains(logged, hexSecret) {
		t.Fatalf("log leaked secret: %s", logged)
	}
	if !strings.Contains(logged, "[redacted]") {
		t.Fatalf("expected redacted path in log: %s", logged)
	}
}

func feedPath(id uuid.UUID, secret []byte) string {
	return "/c/" + id.String() + "/" + hex.EncodeToString(secret)
}

type fakeFeedStore struct {
	feed      store.Feed
	touched   bool
	touchedAt time.Time
}

func (f *fakeFeedStore) GetFeed(_ context.Context, id uuid.UUID) (store.Feed, error) {
	if f.feed.ID != id {
		return store.Feed{}, store.ErrNotFound
	}
	return f.feed, nil
}

func (f *fakeFeedStore) TouchLastRequest(_ context.Context, id uuid.UUID, at time.Time) error {
	if f.feed.ID != id {
		return store.ErrNotFound
	}
	f.touched = true
	f.touchedAt = at
	return nil
}

type fakeRefresh struct {
	body  []byte
	err   error
	calls int
}

func (f *fakeRefresh) Refresh(context.Context, uuid.UUID) ([]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.body, nil
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var _ io.Writer = (*syncBuffer)(nil)
