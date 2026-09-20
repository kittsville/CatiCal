package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"sci1.uk/catical/internal/fetch"
	"sci1.uk/catical/internal/store"
	"sci1.uk/catical/internal/tokens"
)

func TestCreateRateLimitSamePeerReturns429(t *testing.T) {
	st := newMemStore()
	h := New(Config{
		Store:     st,
		Admin:     st,
		Refresh:   &fakeRefresh{body: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")},
		Fetch:     okOriginFetch(),
		Now:       func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) },
		POSTLimit: 3,
	})
	form := url.Values{"name": {"mix"}, "url": {"https://1.1.1.1/a.ics"}}
	for i := 0; i < 3; i++ {
		rec := postFormFrom(t, h, "/", form, "203.0.113.10:40000")
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d unexpectedly 429", i+1)
		}
		if rec.Code != http.StatusOK && rec.Code != http.StatusSeeOther && rec.Code != http.StatusBadRequest {
			t.Fatalf("request %d status %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := postFormFrom(t, h, "/", form, "203.0.113.10:40000")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("N+1 status = %d, want 429", rec.Code)
	}
	other := postFormFrom(t, h, "/", form, "203.0.113.11:40000")
	if other.Code == http.StatusTooManyRequests {
		t.Fatal("different peer should not share the bucket")
	}
}

func TestManagePOSTRateLimitSamePeerReturns429(t *testing.T) {
	st := newMemStore()
	h := New(Config{
		Store:     st,
		Admin:     st,
		Refresh:   &fakeRefresh{body: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")},
		Fetch:     okOriginFetch(),
		Now:       func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) },
		POSTLimit: 3,
	})
	created := postFormFrom(t, h, "/", url.Values{
		"name": {"x"},
		"url":  {"https://1.1.1.1/a.ics"},
	}, "203.0.113.20:1")
	manageM := manageURLRe.FindStringSubmatch(created.Body.String())
	if manageM == nil {
		t.Fatal(created.Body.String())
	}
	path := "/m/" + manageM[1] + "/" + manageM[2]
	save := url.Values{
		"action": {"save"},
		"name":   {"x"},
		"url":    {"https://1.1.1.1/a.ics"},
	}
	for i := 0; i < 3; i++ {
		rec := postFormFrom(t, h, path, save, "203.0.113.21:9")
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("manage POST %d unexpectedly 429", i+1)
		}
	}
	rec := postFormFrom(t, h, path, save, "203.0.113.21:9")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("N+1 manage POST status = %d, want 429", rec.Code)
	}
}

func TestICSGETRateLimitSamePeerReturns429(t *testing.T) {
	id := uuid.New()
	salt, secret, hash, err := tokens.Generate()
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeFeedStore{feed: store.Feed{ID: id, FeedTokenSalt: salt, FeedTokenHash: hash}}
	rf := &fakeRefresh{body: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")}
	h := New(Config{
		Store:    st,
		Refresh:  rf,
		GETLimit: 2,
		Now:      func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) },
	})
	path := feedPath(id, secret) + ".ics"
	for i := 0; i < 2; i++ {
		rec := getFrom(t, h, path, "203.0.113.40:9")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %d status = %d", i+1, rec.Code)
		}
	}
	rec := getFrom(t, h, path, "203.0.113.40:9")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("N+1 ICS GET status = %d, want 429", rec.Code)
	}
	if rf.calls != 2 {
		t.Fatalf("refresh calls = %d, want 2 (429 must not refresh)", rf.calls)
	}
	other := getFrom(t, h, path, "203.0.113.41:9")
	if other.Code == http.StatusTooManyRequests {
		t.Fatal("different peer should not share the ICS GET bucket")
	}
}

func TestCreateOriginFetchLimitDoesNotHitOrigins(t *testing.T) {
	st := newMemStore()
	ft := &countingOriginFetch{}
	h := New(Config{
		Store:       st,
		Admin:       st,
		Refresh:     &fakeRefresh{body: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")},
		Fetch:       ft,
		OriginLimit: 2,
		Now:         func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) },
	})
	form := url.Values{
		"name": {"mix"},
		"url":  {"https://1.1.1.1/a.ics", "https://1.1.1.1/b.ics", "https://1.1.1.1/c.ics"},
	}
	rec := postFormFrom(t, h, "/", form, "203.0.113.60:9")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body %s", rec.Code, rec.Body.String())
	}
	if ft.calls != 0 {
		t.Fatalf("origin GetAll calls = %d, want 0", ft.calls)
	}
	if st.len() != 0 {
		t.Fatal("must not persist a feed after origin quota reject")
	}
}

func TestICSGETOriginQuotaReturns429(t *testing.T) {
	id := uuid.New()
	salt, secret, hash, err := tokens.Generate()
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeFeedStore{feed: store.Feed{ID: id, FeedTokenSalt: salt, FeedTokenHash: hash}}
	rf := &quotaAwareRefresh{body: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")}
	h := New(Config{
		Store:       st,
		Refresh:     rf,
		OriginLimit: 1,
		Now:         func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) },
	})
	path := feedPath(id, secret) + ".ics"
	rec := getFrom(t, h, path, "203.0.113.70:9")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rf.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", rf.calls)
	}
}

func TestICSGETRateLimitPerFeedReturns429(t *testing.T) {
	id := uuid.New()
	salt, secret, hash, err := tokens.Generate()
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeFeedStore{feed: store.Feed{ID: id, FeedTokenSalt: salt, FeedTokenHash: hash}}
	rf := &fakeRefresh{body: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")}
	h := New(Config{
		Store:        st,
		Refresh:      rf,
		GETFeedLimit: 2,
		Now:          func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) },
	})
	path := feedPath(id, secret) + ".ics"
	for i := 0; i < 2; i++ {
		rec := getFrom(t, h, path, "203.0.113."+strconv.Itoa(50+i)+":9")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %d status = %d", i+1, rec.Code)
		}
	}
	rec := getFrom(t, h, path, "203.0.113.99:9")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("N+1 same feed status = %d, want 429", rec.Code)
	}
	if rf.calls != 2 {
		t.Fatalf("refresh calls = %d, want 2", rf.calls)
	}
}

func getFrom(t *testing.T, h http.Handler, path, remote string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRobotsTxtDisallowsManage(t *testing.T) {
	h := New(Config{})
	req := httptest.NewRequest(http.MethodGet, "/robots.txt", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Disallow: /m/") {
		t.Fatalf("robots.txt missing Disallow /m/: %s", body)
	}
	if !strings.Contains(body, "Disallow: /c/") {
		t.Fatalf("robots.txt missing Disallow /c/: %s", body)
	}
}

func TestCreateAndGetLogsNeverContainSourceURLOrTokens(t *testing.T) {
	st := newMemStore()
	buf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(buf, nil))
	source := "https://1.1.1.1/unique-source-leak-check.ics"
	h := New(Config{
		Store:   st,
		Admin:   st,
		Refresh: &fakeRefresh{body: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")},
		Fetch:   okOriginFetch(),
		Now:     func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) },
		Logger:  log,
	})
	created := postForm(t, h, "/", url.Values{
		"name": {"leak-check"},
		"url":  {source},
	})
	if created.Code != http.StatusOK && created.Code != http.StatusSeeOther {
		t.Fatalf("create status %d", created.Code)
	}
	feedM := feedURLRe.FindStringSubmatch(created.Body.String())
	manageM := manageURLRe.FindStringSubmatch(created.Body.String())
	if feedM == nil || manageM == nil {
		t.Fatal(created.Body.String())
	}
	ics := httptest.NewRecorder()
	h.ServeHTTP(ics, httptest.NewRequest(http.MethodGet, "/c/"+feedM[1]+"/"+feedM[2]+".ics", nil))
	if ics.Code != http.StatusOK {
		t.Fatalf("feed GET %d", ics.Code)
	}
	mrec := httptest.NewRecorder()
	h.ServeHTTP(mrec, httptest.NewRequest(http.MethodGet, "/m/"+manageM[1]+"/"+manageM[2], nil))
	if mrec.Code != http.StatusOK {
		t.Fatalf("manage GET %d", mrec.Code)
	}

	logged := buf.String()
	if strings.Contains(logged, source) {
		t.Fatalf("log leaked source URL: %s", logged)
	}
	if strings.Contains(logged, feedM[2]) {
		t.Fatalf("log leaked feed secret: %s", logged)
	}
	if strings.Contains(logged, manageM[2]) {
		t.Fatalf("log leaked manage secret: %s", logged)
	}
}

func postFormFrom(t *testing.T, h http.Handler, path string, form url.Values, remote string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type countingOriginFetch struct {
	calls int
}

func (c *countingOriginFetch) GetAll(_ context.Context, urls []string) []fetch.Result {
	c.calls++
	return okOriginFetch().GetAll(context.Background(), urls)
}

type quotaAwareRefresh struct {
	body  []byte
	calls int
}

func (f *quotaAwareRefresh) Refresh(ctx context.Context, _ uuid.UUID) ([]byte, error) {
	f.calls++
	if err := fetch.ConsumeQuota(ctx, 2); err != nil {
		return nil, err
	}
	return f.body, nil
}
