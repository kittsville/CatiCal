package refresh_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"sci1.uk/catical/internal/fetch"
	"sci1.uk/catical/internal/refresh"
	"sci1.uk/catical/internal/store"
)

func TestFreshMergeSkipsFetcher(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	mergedAt := now.Add(-4*time.Minute - 59*time.Second)
	body := []byte("BEGIN:VCALENDAR\r\nFRESH\r\nEND:VCALENDAR\r\n")
	lastReq := now.Add(-time.Hour)

	st := newMemStore(feedWithMerge(body, mergedAt, lastReq, src("https://example.com/a.ics")))
	ft := &fakeFetch{err: errors.New("should not fetch")}
	svc := refresh.New(st, ft, func() time.Time { return now })

	got, err := svc.Refresh(context.Background(), st.feed.ID)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body: %q", got)
	}
	if ft.calls.Load() != 0 {
		t.Fatalf("fetcher calls: %d", ft.calls.Load())
	}
	if !st.feed.LastRequestAt.Equal(lastReq) {
		t.Fatal("refresh must not call TouchLastRequest")
	}
}

func TestStaleAllOKSavesMerge(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	old := []byte("BEGIN:VCALENDAR\r\nOLD\r\nEND:VCALENDAR\r\n")
	icsA := readICS(t, "calendar_a.ics")

	st := newMemStore(feedWithMerge(old, now.Add(-6*time.Minute), now, src("https://example.com/a.ics")))
	ft := &fakeFetch{byURL: map[string]fetch.Result{
		"https://example.com/a.ics": {Body: icsA, Status: 200},
	}}
	svc := refresh.New(st, ft, func() time.Time { return now })

	got, err := svc.Refresh(context.Background(), st.feed.ID)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if bytes.Contains(got, []byte("OLD")) {
		t.Fatal("expected new merge, not old blob")
	}
	if !bytes.Contains(got, []byte("Meeting A")) {
		t.Fatalf("missing event: %s", got)
	}
	if !bytes.Contains(got, []byte("\r\n")) {
		t.Fatal("expected CRLF")
	}
	if ft.calls.Load() != 1 {
		t.Fatalf("fetcher calls: %d", ft.calls.Load())
	}
	if !bytes.Contains(st.feed.MergedICS, []byte("Meeting A")) {
		t.Fatal("merged blob not saved")
	}
	if st.feed.MergedAt == nil || !st.feed.MergedAt.Equal(now) {
		t.Fatalf("merged_at: %v", st.feed.MergedAt)
	}
	if !bytes.Equal(st.feed.Sources[0].LastICS, icsA) {
		t.Fatal("last_ics not updated")
	}
}

func TestStaleFetchErrorServesOldMerge(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	old := []byte("BEGIN:VCALENDAR\r\nSTALE-OK\r\nEND:VCALENDAR\r\n")
	mergedAt := now.Add(-23 * time.Hour)

	st := newMemStore(feedWithMerge(old, mergedAt, now, src("https://example.com/a.ics")))
	ft := &fakeFetch{err: errors.New("origin down")}
	svc := refresh.New(st, ft, func() time.Time { return now })

	got, err := svc.Refresh(context.Background(), st.feed.ID)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !bytes.Equal(got, old) {
		t.Fatalf("want old merge, got %q", got)
	}
	if ft.calls.Load() != 1 {
		t.Fatalf("fetcher calls: %d", ft.calls.Load())
	}
	if st.feed.Sources[0].LastError == nil || *st.feed.Sources[0].LastError == "" {
		t.Fatal("expected last_error on source")
	}
	if bytes.Contains([]byte(*st.feed.Sources[0].LastError), []byte("https://")) {
		t.Fatalf("last_error must not include URL: %s", *st.feed.Sources[0].LastError)
	}
	if !bytes.Equal(st.feed.MergedICS, old) {
		t.Fatal("must not replace merged blob on total fetch failure")
	}
}

func TestStaleFetchErrorExpiredFails(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	old := []byte("BEGIN:VCALENDAR\r\nTOO-OLD\r\nEND:VCALENDAR\r\n")
	mergedAt := now.Add(-24*time.Hour - time.Second)

	st := newMemStore(feedWithMerge(old, mergedAt, now, src("https://example.com/a.ics")))
	ft := &fakeFetch{err: errors.New("origin down")}
	svc := refresh.New(st, ft, func() time.Time { return now })

	got, err := svc.Refresh(context.Background(), st.feed.ID)
	if err == nil {
		t.Fatal("expected error")
	}
	if len(got) != 0 {
		t.Fatalf("must not return body: %q", got)
	}
	if ft.calls.Load() != 1 {
		t.Fatalf("fetcher calls: %d", ft.calls.Load())
	}
}

func TestPartialFetchUsesLastGood(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	icsA := readICS(t, "calendar_a.ics")
	icsB := readICS(t, "calendar_b.ics")
	prevBSuccess := now.Add(-2 * time.Hour)

	a := src("https://example.com/a.ics")
	b := src("https://example.com/b.ics")
	b.LastICS = icsB
	b.LastSuccessAt = &prevBSuccess

	oldMerge := []byte("BEGIN:VCALENDAR\r\nOLD-MERGE\r\nEND:VCALENDAR\r\n")
	st := newMemStore(feedWithMerge(oldMerge, now.Add(-10*time.Minute), now, a, b))
	ft := &fakeFetch{byURL: map[string]fetch.Result{
		"https://example.com/a.ics": {Body: icsA, Status: 200},
		"https://example.com/b.ics": {Err: errors.New("500"), Status: 500},
	}}
	svc := refresh.New(st, ft, func() time.Time { return now })

	got, err := svc.Refresh(context.Background(), st.feed.ID)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !bytes.Contains(got, []byte("Meeting A")) || !bytes.Contains(got, []byte("Meeting B")) {
		t.Fatalf("merge should use new A and last-good B: %s", got)
	}
	if !bytes.Equal(st.feed.Sources[0].LastICS, icsA) {
		t.Fatal("A last_ics should update")
	}
	if !bytes.Equal(st.feed.Sources[1].LastICS, icsB) {
		t.Fatal("B last_ics should be kept")
	}
	if st.feed.Sources[1].LastError == nil {
		t.Fatal("B last_error should be recorded")
	}
	if st.feed.Sources[1].LastSuccessAt == nil || !st.feed.Sources[1].LastSuccessAt.Equal(prevBSuccess) {
		t.Fatalf("B last_success_at: %v", st.feed.Sources[1].LastSuccessAt)
	}
}

func TestSingleflightOneFetchBurst(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	icsA := readICS(t, "calendar_a.ics")
	st := newMemStore(feedWithMerge(nil, time.Time{}, now, src("https://example.com/a.ics")))
	ft := &fakeFetch{
		delay: 80 * time.Millisecond,
		byURL: map[string]fetch.Result{
			"https://example.com/a.ics": {Body: icsA, Status: 200},
		},
	}
	svc := refresh.New(st, ft, func() time.Time { return now })

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Refresh(context.Background(), st.feed.ID)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if ft.calls.Load() != 1 {
		t.Fatalf("expected one fetch burst, got %d", ft.calls.Load())
	}
}

func TestAllOriginsRateLimited(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	st := newMemStore(feedWithMerge(nil, time.Time{}, now, src("https://example.com/a.ics")))
	ft := &fakeFetch{body: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")}
	svc := refresh.New(st, ft, func() time.Time { return now })
	ctx := fetch.WithQuota(context.Background(), func(n int) bool { return false })

	_, err := svc.Refresh(ctx, st.feed.ID)
	if !errors.Is(err, fetch.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if ft.calls.Load() != 0 {
		t.Fatalf("fetcher calls = %d, want 0", ft.calls.Load())
	}
}

func TestEmptyMergeError(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	st := newMemStore(feedWithMerge(nil, time.Time{}, now, src("https://example.com/a.ics")))
	ft := &fakeFetch{err: errors.New("down")}
	svc := refresh.New(st, ft, func() time.Time { return now })

	got, err := svc.Refresh(context.Background(), st.feed.ID)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, refresh.ErrNoUsableSources) {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("body: %q", got)
	}
}

func TestMaxEightSources(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	icsA := readICS(t, "calendar_a.ics")
	sources := make([]store.Source, 9)
	for i := range sources {
		sources[i] = src("https://example.com/" + string(rune('a'+i)) + ".ics")
	}
	st := newMemStore(feedWithMerge(nil, time.Time{}, now, sources...))
	ft := &fakeFetch{body: icsA}
	svc := refresh.New(st, ft, func() time.Time { return now })

	if _, err := svc.Refresh(context.Background(), st.feed.ID); err != nil {
		t.Fatal(err)
	}
	if len(ft.lastURLs) != 8 {
		t.Fatalf("fetched %d urls, want 8", len(ft.lastURLs))
	}
}

type memStore struct {
	mu   sync.Mutex
	feed store.Feed
}

func newMemStore(f store.Feed) *memStore {
	return &memStore{feed: f}
}

func (m *memStore) GetFeed(_ context.Context, id uuid.UUID) (store.Feed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.feed.ID != id {
		return store.Feed{}, store.ErrNotFound
	}
	return cloneFeed(m.feed), nil
}

func (m *memStore) UpdateMerged(_ context.Context, id uuid.UUID, ics []byte, mergedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.feed.ID != id {
		return store.ErrNotFound
	}
	m.feed.MergedICS = append([]byte(nil), ics...)
	t := mergedAt
	m.feed.MergedAt = &t
	return nil
}

func (m *memStore) UpdateSourceFetch(_ context.Context, src store.Source) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, s := range m.feed.Sources {
		if s.ID == src.ID {
			m.feed.Sources[i] = src
			return nil
		}
	}
	return store.ErrNotFound
}

type fakeFetch struct {
	calls    atomic.Int32
	delay    time.Duration
	byURL    map[string]fetch.Result
	err      error
	body     []byte
	lastURLs []string
	mu       sync.Mutex
}

func (f *fakeFetch) GetAll(ctx context.Context, urls []string) []fetch.Result {
	if err := fetch.ConsumeQuota(ctx, len(urls)); err != nil {
		out := make([]fetch.Result, len(urls))
		for i := range out {
			out[i].Err = err
		}
		return out
	}
	f.calls.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	f.lastURLs = append([]string(nil), urls...)
	f.mu.Unlock()
	out := make([]fetch.Result, len(urls))
	for i, u := range urls {
		if f.byURL != nil {
			if r, ok := f.byURL[u]; ok {
				out[i] = r
				continue
			}
		}
		if f.err != nil {
			out[i] = fetch.Result{Err: f.err}
			continue
		}
		out[i] = fetch.Result{Body: f.body, Status: 200}
	}
	return out
}

func src(url string) store.Source {
	return store.Source{
		ID:       uuid.New(),
		URL:      url,
		Position: 0,
		Label:    "L",
	}
}

func feedWithMerge(body []byte, mergedAt, lastReq time.Time, sources ...store.Source) store.Feed {
	id := uuid.New()
	for i := range sources {
		sources[i].FeedID = id
		sources[i].Position = i
	}
	f := store.Feed{
		ID:            id,
		Name:          "Mix",
		LastRequestAt: lastReq,
		Sources:       sources,
	}
	if len(body) > 0 {
		f.MergedICS = body
		t := mergedAt
		f.MergedAt = &t
	}
	return f
}

func cloneFeed(f store.Feed) store.Feed {
	out := f
	out.MergedICS = append([]byte(nil), f.MergedICS...)
	out.Sources = append([]store.Source(nil), f.Sources...)
	for i := range out.Sources {
		out.Sources[i].LastICS = append([]byte(nil), f.Sources[i].LastICS...)
	}
	return out
}

func readICS(t *testing.T, name string) []byte {
	t.Helper()
	p := filepath.Join("..", "..", "testdata", "ics", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
