package httpserver

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"sci1.uk/catical/internal/store"
	"sci1.uk/catical/internal/tokens"
)

var (
	feedURLRe   = regexp.MustCompile(`https://catical\.sci1\.uk/c/([0-9a-f-]+)/([0-9a-f]+)\.ics`)
	manageURLRe = regexp.MustCompile(`https://catical\.sci1\.uk/m/([0-9a-f-]+)/([0-9a-f]+)`)
)

func TestCreateFormGETHeadersAndNoindex(t *testing.T) {
	h := New(Config{Admin: newMemStore()})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q", rec.Header().Get("Referrer-Policy"))
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("X-Frame-Options = %q", rec.Header().Get("X-Frame-Options"))
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", rec.Header().Get("X-Content-Type-Options"))
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'none'") {
		t.Fatalf("CSP should disable JS: %q", csp)
	}
	if strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Fatalf("CSP allows inline JS: %q", csp)
	}
	if !strings.Contains(csp, "https://unpkg.com") {
		t.Fatalf("CSP style-src should allow MDC CDN: %q", csp)
	}
	body := rec.Body.String()
	if !strings.Contains(strings.ToLower(body), "noindex") {
		t.Fatal("expected noindex")
	}
	if !strings.Contains(body, `name="name"`) || !strings.Contains(body, `name="url"`) {
		t.Fatal("expected name and url fields")
	}
	if !strings.Contains(body, mdcCSSURL) {
		t.Fatal("expected Material Components Web stylesheet")
	}
	if strings.Contains(body, "<script") {
		t.Fatal("no JS")
	}
}

func TestPOSTCreateSuccessContainsBothLinks(t *testing.T) {
	st := newMemStore()
	h := htmlHandler(st)
	rec := postForm(t, h, "/", url.Values{
		"name":             {"Work+Home"},
		"prefix_summaries": {"on"},
		"url":              {"https://1.1.1.1/a.ics", "https://1.1.1.1/b.ics"},
		"label":            {"A", "B"},
	})
	if rec.Code != http.StatusOK && rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	feedM := feedURLRe.FindStringSubmatch(body)
	manageM := manageURLRe.FindStringSubmatch(body)
	if feedM == nil || manageM == nil {
		t.Fatalf("missing catical.sci1.uk links in %s", body)
	}
	id, err := uuid.Parse(feedM[1])
	if err != nil {
		t.Fatal(err)
	}
	feedSecret, err := hex.DecodeString(feedM[2])
	if err != nil {
		t.Fatal(err)
	}
	feed, err := st.GetFeed(context.Background(), id)
	if err != nil {
		t.Fatalf("db row: %v", err)
	}
	if len(feed.Sources) != 2 {
		t.Fatalf("sources = %d", len(feed.Sources))
	}
	if !tokens.Verify(feed.FeedTokenSalt, feed.FeedTokenHash, feedSecret) {
		t.Fatal("stored hash must verify feed secret")
	}
	if bytesEqual(feed.FeedTokenHash, feedSecret) || bytesEqual(feed.FeedTokenSalt, feedSecret) {
		t.Fatal("must not store plaintext secret")
	}
	if !strings.Contains(strings.ToLower(body), "unrecoverable") && !strings.Contains(strings.ToLower(body), "not be shown") {
		t.Fatal("expected unrecoverable / not shown again warning")
	}

	managePath := "/m/" + manageM[1] + "/" + manageM[2]
	req := httptest.NewRequest(http.MethodGet, managePath, nil)
	mrec := httptest.NewRecorder()
	h.ServeHTTP(mrec, req)
	if mrec.Code != http.StatusOK {
		t.Fatalf("manage GET %d", mrec.Code)
	}
	if strings.Contains(mrec.Body.String(), feedM[2]) {
		t.Fatal("manage GET must not reprint the feed secret")
	}
}

func TestManageGETWrongSecretIs404(t *testing.T) {
	st := newMemStore()
	h := htmlHandler(st)
	created := postForm(t, h, "/", url.Values{
		"name": {"x"},
		"url":  {"https://1.1.1.1/a.ics"},
	})
	manageM := manageURLRe.FindStringSubmatch(created.Body.String())
	if manageM == nil {
		t.Fatal(created.Body.String())
	}
	_, other, _, err := tokens.Generate()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/m/"+manageM[1]+"/"+hex.EncodeToString(other), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestPOSTSaveChangesNameAndSources(t *testing.T) {
	st := newMemStore()
	h := htmlHandler(st)
	created := postForm(t, h, "/", url.Values{
		"name": {"old"},
		"url":  {"https://1.1.1.1/a.ics"},
	})
	manageM := manageURLRe.FindStringSubmatch(created.Body.String())
	path := "/m/" + manageM[1] + "/" + manageM[2]
	rec := postForm(t, h, path, url.Values{
		"action":           {"save"},
		"name":             {"new-name"},
		"prefix_summaries": {"on"},
		"url":              {"https://1.1.1.1/z.ics"},
		"label":            {"Z"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	id, _ := uuid.Parse(manageM[1])
	feed, err := st.GetFeed(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if feed.Name != "new-name" || !feed.PrefixSummaries {
		t.Fatalf("feed %+v", feed)
	}
	if len(feed.Sources) != 1 || feed.Sources[0].URL != "https://1.1.1.1/z.ics" {
		t.Fatalf("sources %+v", feed.Sources)
	}
}

func TestPOSTDeleteRemovesFeed(t *testing.T) {
	st := newMemStore()
	h := htmlHandler(st)
	created := postForm(t, h, "/", url.Values{
		"name": {"gone"},
		"url":  {"https://1.1.1.1/a.ics"},
	})
	feedM := feedURLRe.FindStringSubmatch(created.Body.String())
	manageM := manageURLRe.FindStringSubmatch(created.Body.String())
	path := "/m/" + manageM[1] + "/" + manageM[2]
	rec := postForm(t, h, path, url.Values{"action": {"delete"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Fatalf("Location = %q", loc)
	}
	id, _ := uuid.Parse(feedM[1])
	if _, err := st.GetFeed(context.Background(), id); err != store.ErrNotFound {
		t.Fatalf("expected gone: %v", err)
	}
	ics := httptest.NewRecorder()
	h.ServeHTTP(ics, httptest.NewRequest(http.MethodGet, "/c/"+feedM[1]+"/"+feedM[2]+".ics", nil))
	if ics.Code != http.StatusNotFound {
		t.Fatalf("feed GET status = %d", ics.Code)
	}
}

func TestRotateFeedToken(t *testing.T) {
	st := newMemStore()
	h := htmlHandler(st)
	created := postForm(t, h, "/", url.Values{
		"name": {"r"},
		"url":  {"https://1.1.1.1/a.ics"},
	})
	oldFeed := feedURLRe.FindStringSubmatch(created.Body.String())
	manageM := manageURLRe.FindStringSubmatch(created.Body.String())
	path := "/m/" + manageM[1] + "/" + manageM[2]
	rec := postForm(t, h, path, url.Values{"action": {"rotate_feed"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	newFeed := feedURLRe.FindStringSubmatch(rec.Body.String())
	if newFeed == nil {
		t.Fatalf("expected new feed URL: %s", rec.Body.String())
	}
	if newFeed[2] == oldFeed[2] {
		t.Fatal("feed secret unchanged")
	}
	oldICS := httptest.NewRecorder()
	h.ServeHTTP(oldICS, httptest.NewRequest(http.MethodGet, "/c/"+oldFeed[1]+"/"+oldFeed[2]+".ics", nil))
	if oldICS.Code != http.StatusNotFound {
		t.Fatalf("old feed %d", oldICS.Code)
	}
	newICS := httptest.NewRecorder()
	h.ServeHTTP(newICS, httptest.NewRequest(http.MethodGet, "/c/"+newFeed[1]+"/"+newFeed[2]+".ics", nil))
	if newICS.Code != http.StatusOK {
		t.Fatalf("new feed %d", newICS.Code)
	}
	// manage URL unchanged
	mrec := httptest.NewRecorder()
	h.ServeHTTP(mrec, httptest.NewRequest(http.MethodGet, path, nil))
	if mrec.Code != http.StatusOK {
		t.Fatalf("manage still %d", mrec.Code)
	}
}

func TestRotateManageToken(t *testing.T) {
	st := newMemStore()
	h := htmlHandler(st)
	created := postForm(t, h, "/", url.Values{
		"name": {"r"},
		"url":  {"https://1.1.1.1/a.ics"},
	})
	manageM := manageURLRe.FindStringSubmatch(created.Body.String())
	oldPath := "/m/" + manageM[1] + "/" + manageM[2]
	rec := postForm(t, h, oldPath, url.Values{"action": {"rotate_manage"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	newM := manageURLRe.FindStringSubmatch(rec.Body.String())
	if newM == nil || newM[2] == manageM[2] {
		t.Fatalf("expected new manage URL: %s", rec.Body.String())
	}
	oldGET := httptest.NewRecorder()
	h.ServeHTTP(oldGET, httptest.NewRequest(http.MethodGet, oldPath, nil))
	if oldGET.Code != http.StatusNotFound {
		t.Fatalf("old manage %d", oldGET.Code)
	}
}

func TestCreateRejectsInvalidInput(t *testing.T) {
	st := newMemStore()
	h := htmlHandler(st)
	urls9 := make([]string, 9)
	for i := range urls9 {
		urls9[i] = "https://1.1.1.1/" + strconv.Itoa(i) + ".ics"
	}
	cases := []struct {
		name string
		form url.Values
	}{
		{"empty name", url.Values{"name": {""}, "url": {"https://1.1.1.1/a.ics"}}},
		{"http url", url.Values{"name": {"n"}, "url": {"http://example.com/a.ics"}}},
		{"ssrf", url.Values{"name": {"n"}, "url": {"https://127.0.0.1/a.ics"}}},
		{"too many", url.Values{"name": {"n"}, "url": urls9}},
	}
	before := st.len()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postForm(t, h, "/", tc.form)
			if rec.Code < 400 {
				t.Fatalf("status = %d, want 4xx, body %s", rec.Code, rec.Body.String())
			}
		})
	}
	if st.len() != before {
		t.Fatalf("rejects must not insert, have %d", st.len())
	}
}

func htmlHandler(st *memStore) http.Handler {
	return New(Config{
		Store:   st,
		Admin:   st,
		Refresh: &fakeRefresh{body: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")},
		Now:     func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) },
	})
}

func postForm(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type memStore struct {
	mu    sync.Mutex
	feeds map[uuid.UUID]store.Feed
}

func newMemStore() *memStore {
	return &memStore{feeds: make(map[uuid.UUID]store.Feed)}
}

func (m *memStore) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.feeds)
}

func cloneFeed(f store.Feed) store.Feed {
	f.Sources = append([]store.Source(nil), f.Sources...)
	return f
}

func (m *memStore) CreateFeed(_ context.Context, p store.CreateFeedParams) (store.Feed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := uuid.New()
	srcs := make([]store.Source, len(p.Sources))
	for i, s := range p.Sources {
		srcs[i] = store.Source{
			ID:       uuid.New(),
			FeedID:   id,
			URL:      s.URL,
			Label:    s.Label,
			Position: s.Position,
		}
	}
	f := store.Feed{
		ID:              id,
		Name:            p.Name,
		PrefixSummaries: p.PrefixSummaries,
		FeedTokenSalt:   append([]byte(nil), p.FeedTokenSalt...),
		FeedTokenHash:   append([]byte(nil), p.FeedTokenHash...),
		ManageTokenSalt: append([]byte(nil), p.ManageTokenSalt...),
		ManageTokenHash: append([]byte(nil), p.ManageTokenHash...),
		LastRequestAt:   p.LastRequestAt,
		Sources:         srcs,
	}
	m.feeds[id] = f
	return cloneFeed(f), nil
}

func (m *memStore) GetFeed(_ context.Context, id uuid.UUID) (store.Feed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.feeds[id]
	if !ok {
		return store.Feed{}, store.ErrNotFound
	}
	return cloneFeed(f), nil
}

func (m *memStore) TouchLastRequest(_ context.Context, id uuid.UUID, at time.Time) error {
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

func (m *memStore) UpdateFeed(_ context.Context, id uuid.UUID, p store.UpdateFeedParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.feeds[id]
	if !ok {
		return store.ErrNotFound
	}
	f.Name = p.Name
	f.PrefixSummaries = p.PrefixSummaries
	f.MergedICS = nil
	f.MergedAt = nil
	srcs := make([]store.Source, len(p.Sources))
	for i, s := range p.Sources {
		srcs[i] = store.Source{
			ID: uuid.New(), FeedID: id, URL: s.URL, Label: s.Label, Position: s.Position,
		}
	}
	f.Sources = srcs
	m.feeds[id] = f
	return nil
}

func (m *memStore) DeleteFeed(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.feeds[id]; !ok {
		return store.ErrNotFound
	}
	delete(m.feeds, id)
	return nil
}

func (m *memStore) RotateFeedToken(_ context.Context, id uuid.UUID, salt, hash []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.feeds[id]
	if !ok {
		return store.ErrNotFound
	}
	f.FeedTokenSalt = append([]byte(nil), salt...)
	f.FeedTokenHash = append([]byte(nil), hash...)
	m.feeds[id] = f
	return nil
}

func (m *memStore) RotateManageToken(_ context.Context, id uuid.UUID, salt, hash []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.feeds[id]
	if !ok {
		return store.ErrNotFound
	}
	f.ManageTokenSalt = append([]byte(nil), salt...)
	f.ManageTokenHash = append([]byte(nil), hash...)
	m.feeds[id] = f
	return nil
}
