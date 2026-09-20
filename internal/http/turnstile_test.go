package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type stubVerifier struct {
	ok    bool
	calls atomic.Int32
	last  string
}

func (s *stubVerifier) Verify(_ context.Context, token, _ string) error {
	s.calls.Add(1)
	s.last = token
	if !s.ok {
		return errors.New("turnstile failed")
	}
	return nil
}

func turnstileHandler(st *memStore, secret string, v TurnstileVerifier) http.Handler {
	return New(Config{
		Store:             st,
		Admin:             st,
		Refresh:           &fakeRefresh{body: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")},
		Now:               func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) },
		TurnstileSecret:   secret,
		TurnstileSiteKey:  "site-key-test",
		TurnstileVerifier: v,
	})
}

func TestTurnstileMissingTokenNoCreate(t *testing.T) {
	st := newMemStore()
	v := &stubVerifier{ok: true}
	h := turnstileHandler(st, "secret", v)
	rec := postForm(t, h, "/", url.Values{
		"name": {"mix"},
		"url":  {"https://1.1.1.1/a.ics"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body %s", rec.Code, rec.Body.String())
	}
	if st.len() != 0 {
		t.Fatal("must not write feed without token")
	}
	if v.calls.Load() != 0 {
		t.Fatal("verifier must not run without a token")
	}
}

func TestTurnstileOKAllowsCreate(t *testing.T) {
	st := newMemStore()
	v := &stubVerifier{ok: true}
	h := turnstileHandler(st, "secret", v)
	rec := postForm(t, h, "/", url.Values{
		"name":                  {"mix"},
		"url":                   {"https://1.1.1.1/a.ics"},
		"cf-turnstile-response": {"ok-token"},
	})
	if rec.Code != http.StatusOK && rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if st.len() != 1 {
		t.Fatalf("want 1 feed, have %d", st.len())
	}
	if v.calls.Load() != 1 || v.last != "ok-token" {
		t.Fatalf("verifier calls=%d token=%q", v.calls.Load(), v.last)
	}
}

func TestTurnstileFailNoCreate(t *testing.T) {
	st := newMemStore()
	v := &stubVerifier{ok: false}
	h := turnstileHandler(st, "secret", v)
	rec := postForm(t, h, "/", url.Values{
		"name":                  {"mix"},
		"url":                   {"https://1.1.1.1/a.ics"},
		"cf-turnstile-response": {"bad"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body %s", rec.Code, rec.Body.String())
	}
	if st.len() != 0 {
		t.Fatal("must not write feed on failed verify")
	}
}

func TestTurnstileOffWhenSecretEmpty(t *testing.T) {
	st := newMemStore()
	v := &stubVerifier{ok: false}
	h := New(Config{
		Store:             st,
		Admin:             st,
		Refresh:           &fakeRefresh{body: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")},
		Now:               func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) },
		TurnstileVerifier: v,
	})
	rec := postForm(t, h, "/", url.Values{
		"name": {"mix"},
		"url":  {"https://1.1.1.1/a.ics"},
	})
	if rec.Code != http.StatusOK && rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if st.len() != 1 {
		t.Fatal("create should proceed when TURNSTILE_SECRET is unset")
	}
	if v.calls.Load() != 0 {
		t.Fatal("verifier must not run when secret is empty")
	}
}

func TestTurnstileGatesManagePOSTNotICSGET(t *testing.T) {
	st := newMemStore()
	hOff := htmlHandler(st)
	created := postForm(t, hOff, "/", url.Values{
		"name": {"x"},
		"url":  {"https://1.1.1.1/a.ics"},
	})
	feedM := feedURLRe.FindStringSubmatch(created.Body.String())
	manageM := manageURLRe.FindStringSubmatch(created.Body.String())
	if feedM == nil || manageM == nil {
		t.Fatal(created.Body.String())
	}
	path := "/m/" + manageM[1] + "/" + manageM[2]

	v := &stubVerifier{ok: false}
	hOn := turnstileHandler(st, "secret", v)
	rec := postForm(t, hOn, path, url.Values{
		"action":                {"save"},
		"name":                  {"changed"},
		"url":                   {"https://1.1.1.1/a.ics"},
		"cf-turnstile-response": {"bad"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("manage POST status = %d, want 403", rec.Code)
	}

	ics := httptest.NewRecorder()
	hOn.ServeHTTP(ics, httptest.NewRequest(http.MethodGet, "/c/"+feedM[1]+"/"+feedM[2]+".ics", nil))
	if ics.Code != http.StatusOK {
		t.Fatalf("ICS GET must stay token-only, status = %d", ics.Code)
	}
}

func TestTurnstileWidgetAndCSPWhenSiteKeySet(t *testing.T) {
	h := New(Config{Admin: newMemStore(), TurnstileSiteKey: "site-key-test"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "https://challenges.cloudflare.com") {
		t.Fatalf("CSP must allow Turnstile: %q", csp)
	}
	if strings.Contains(csp, "script-src 'none'") {
		t.Fatalf("script-src none blocks widget: %q", csp)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "challenges.cloudflare.com/turnstile") {
		t.Fatal("expected Turnstile script")
	}
	if !strings.Contains(body, `data-sitekey="site-key-test"`) {
		t.Fatal("expected site key on widget")
	}
}

func TestSiteverifyHTTP(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	t.Cleanup(srv.Close)
	v := newSiteverify(srv.Client(), srv.URL, "sec")
	if err := v.Verify(context.Background(), "tok", "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, "secret=sec") || !strings.Contains(gotBody, "response=tok") {
		t.Fatalf("siteverify body = %q", gotBody)
	}

	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false})
	}))
	t.Cleanup(failSrv.Close)
	bad := newSiteverify(failSrv.Client(), failSrv.URL, "sec")
	if err := bad.Verify(context.Background(), "tok", ""); err == nil {
		t.Fatal("expected failure")
	}
}
