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
	ok     bool
	calls  atomic.Int32
	last   string
	action string
}

func (s *stubVerifier) Verify(_ context.Context, token, _, action string) error {
	s.calls.Add(1)
	s.last = token
	s.action = action
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
	if v.calls.Load() != 1 || v.last != "ok-token" || v.action != "create" {
		t.Fatalf("verifier calls=%d token=%q action=%q", v.calls.Load(), v.last, v.action)
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
	if v.action != "manage" {
		t.Fatalf("manage POST action = %q, want manage", v.action)
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
	if !strings.Contains(body, `data-action="create"`) {
		t.Fatal("expected create action on widget")
	}
}

func TestTurnstileManageWidgetAction(t *testing.T) {
	st := newMemStore()
	hOff := htmlHandler(st)
	created := postForm(t, hOff, "/", url.Values{
		"name": {"x"},
		"url":  {"https://1.1.1.1/a.ics"},
	})
	manageM := manageURLRe.FindStringSubmatch(created.Body.String())
	if manageM == nil {
		t.Fatal(created.Body.String())
	}
	h := New(Config{
		Admin:            st,
		TurnstileSiteKey: "site-key-test",
	})
	req := httptest.NewRequest(http.MethodGet, "/m/"+manageM[1]+"/"+manageM[2], nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `data-action="manage"`) {
		t.Fatal("expected manage action on widget")
	}
}

func TestTurnstileTokenTooLong(t *testing.T) {
	st := newMemStore()
	v := &stubVerifier{ok: true}
	h := turnstileHandler(st, "secret", v)
	rec := postForm(t, h, "/", url.Values{
		"name":                  {"mix"},
		"url":                   {"https://1.1.1.1/a.ics"},
		"cf-turnstile-response": {strings.Repeat("x", 2049)},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if st.len() != 0 {
		t.Fatal("must not write feed with oversized token")
	}
	if v.calls.Load() != 0 {
		t.Fatal("verifier must not run for oversized token")
	}
}

func TestSiteverifyHTTP(t *testing.T) {
	hosts := map[string]struct{}{"catical.sci1.uk": {}}
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":  true,
			"action":   "create",
			"hostname": "catical.sci1.uk",
		})
	}))
	t.Cleanup(srv.Close)
	v := newSiteverify(srv.Client(), srv.URL, "sec", hosts)
	if err := v.Verify(context.Background(), "tok", "203.0.113.9", "create"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, "secret=sec") || !strings.Contains(gotBody, "response=tok") {
		t.Fatalf("siteverify body = %q", gotBody)
	}

	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false})
	}))
	t.Cleanup(failSrv.Close)
	bad := newSiteverify(failSrv.Client(), failSrv.URL, "sec", hosts)
	if err := bad.Verify(context.Background(), "tok", "", "create"); err == nil {
		t.Fatal("expected failure")
	}

	mismatch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":  true,
			"action":   "other",
			"hostname": "catical.sci1.uk",
		})
	}))
	t.Cleanup(mismatch.Close)
	wrongAction := newSiteverify(mismatch.Client(), mismatch.URL, "sec", hosts)
	if err := wrongAction.Verify(context.Background(), "tok", "", "create"); err == nil {
		t.Fatal("expected action mismatch")
	}

	wrongHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":  true,
			"action":   "create",
			"hostname": "evil.example",
		})
	}))
	t.Cleanup(wrongHost.Close)
	hostClient := newSiteverify(wrongHost.Client(), wrongHost.URL, "sec", hosts)
	if err := hostClient.Verify(context.Background(), "tok", "", "create"); err == nil {
		t.Fatal("expected hostname mismatch")
	}
}

func TestExpectedHostnamesFromBaseURL(t *testing.T) {
	h := (Config{BaseURL: "https://catical.sci1.uk"}).expectedHostnames()
	if _, ok := h["catical.sci1.uk"]; !ok {
		t.Fatalf("got %#v", h)
	}
	local := (Config{BaseURL: "http://localhost:8080"}).expectedHostnames()
	if len(local) != 0 {
		t.Fatalf("loopback must not be implied: %#v", local)
	}
	explicit := (Config{TurnstileHostnames: "localhost, catical.sci1.uk"}).expectedHostnames()
	if _, ok := explicit["localhost"]; !ok {
		t.Fatal("explicit localhost allowed for local/dev")
	}
}
