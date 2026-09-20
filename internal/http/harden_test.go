package httpserver

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
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
