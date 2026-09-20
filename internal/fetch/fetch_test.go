package fetch

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testFetcher(t *testing.T, servers ...*httptest.Server) *Fetcher {
	t.Helper()
	f := New()
	f.Timeout = 5 * time.Second
	for _, s := range servers {
		u, err := url.Parse(s.URL)
		if err != nil {
			t.Fatal(err)
		}
		f.PermitHost(u.Host)
	}
	return f
}

func TestGetAllParallelTwoServers(t *testing.T) {
	var seen atomic.Int32
	release := make(chan struct{})
	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		<-release
		io.WriteString(w, "BEGIN:VCALENDAR\nCAL:A")
	}))
	defer s1.Close()
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		<-release
		io.WriteString(w, "BEGIN:VCALENDAR\nCAL:B")
	}))
	defer s2.Close()

	f := testFetcher(t, s1, s2)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	errCh := make(chan []Result, 1)
	go func() {
		errCh <- f.GetAll(ctx, []string{s1.URL + "/a.ics", s2.URL + "/b.ics"})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for seen.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("handlers were not entered in parallel")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)

	results := <-errCh
	if len(results) != 2 {
		t.Fatalf("len(results)=%d", len(results))
	}
	if results[0].Err != nil || !bytes.Contains(results[0].Body, []byte("CAL:A")) {
		t.Fatalf("server A: status=%d err=%v body=%q", results[0].Status, results[0].Err, results[0].Body)
	}
	if results[1].Err != nil || !bytes.Contains(results[1].Body, []byte("CAL:B")) {
		t.Fatalf("server B: status=%d err=%v body=%q", results[1].Status, results[1].Err, results[1].Body)
	}
}

func TestGetAllPartialFailure(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok-body")
	}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer bad.Close()

	results := testFetcher(t, ok, bad).GetAll(context.Background(), []string{ok.URL, bad.URL})
	if len(results) != 2 {
		t.Fatalf("len=%d", len(results))
	}
	if results[0].Err != nil || string(results[0].Body) != "ok-body" {
		t.Fatalf("200 result: %+v", results[0])
	}
	if results[1].Err == nil {
		t.Fatal("expected error for 500")
	}
	if results[1].Status != http.StatusInternalServerError {
		t.Fatalf("status=%d", results[1].Status)
	}
}

func TestGetAllTimeout(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
		io.WriteString(w, "late")
	}))
	defer s.Close()

	f := testFetcher(t, s)
	f.Timeout = 50 * time.Millisecond
	start := time.Now()
	results := f.GetAll(context.Background(), []string{s.URL})
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("timeout took too long")
	}
	if results[0].Err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestGetAllBodyCap(t *testing.T) {
	const capBytes = 64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), capBytes+1))
	}))
	defer s.Close()

	f := testFetcher(t, s)
	f.MaxBody = capBytes
	results := f.GetAll(context.Background(), []string{s.URL})
	if results[0].Err == nil {
		t.Fatal("expected oversize body error")
	}
	if int64(len(results[0].Body)) > capBytes {
		t.Fatalf("buffered %d bytes, cap %d", len(results[0].Body), capBytes)
	}
}

func TestGetAllRejectsSSRF(t *testing.T) {
	f := New()
	blocked := []string{
		"file:///etc/passwd",
		"ftp://example.com/cal.ics",
		"gopher://example.com/",
		"http://localhost/cal.ics",
		"https://localhost/cal.ics",
		"http://LOCALHOST/cal.ics",
		"http://127.0.0.1/cal.ics",
		"http://127.0.0.1:8080/cal.ics",
		"http://[::1]/cal.ics",
		"http://10.0.0.1/cal.ics",
		"http://10.255.255.254/x",
		"http://172.16.0.1/cal.ics",
		"http://172.31.255.1/cal.ics",
		"http://192.168.1.1/cal.ics",
		"http://169.254.1.1/cal.ics",
		"http://169.254.169.254/latest/meta-data/",
		"http://metadata.google.internal/",
	}
	for _, raw := range blocked {
		t.Run(raw, func(t *testing.T) {
			results := f.GetAll(context.Background(), []string{raw})
			if results[0].Err == nil {
				t.Fatalf("expected reject for %s", raw)
			}
		})
	}
}

func TestGetAllRedirectToBlocked(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer s.Close()

	results := testFetcher(t, s).GetAll(context.Background(), []string{s.URL})
	if results[0].Err == nil {
		t.Fatal("expected redirect SSRF error")
	}
}

func TestGetAllRedirectToLoopback(t *testing.T) {
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "internal-secret")
	}))
	defer victim.Close()

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, victim.URL, http.StatusFound)
	}))
	defer s.Close()

	// Only the first server is permitted; following to another loopback must fail.
	results := testFetcher(t, s).GetAll(context.Background(), []string{s.URL})
	if results[0].Err == nil {
		t.Fatal("expected blocked redirect to unpermitted loopback")
	}
	if bytes.Contains(results[0].Body, []byte("internal-secret")) {
		t.Fatal("followed redirect into loopback body")
	}
}

func TestGetAllAllowsHTTPTestServer(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method %s", r.Method)
		}
		io.WriteString(w, "BEGIN:VCALENDAR")
	}))
	defer s.Close()

	results := testFetcher(t, s).GetAll(context.Background(), []string{s.URL})
	if results[0].Err != nil {
		t.Fatal(results[0].Err)
	}
	if !bytes.Contains(results[0].Body, []byte("BEGIN:VCALENDAR")) {
		t.Fatalf("body=%q", results[0].Body)
	}
}

func TestGetAllMaxSources(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer s.Close()

	urls := make([]string, MaxSources+1)
	for i := range urls {
		urls[i] = s.URL
	}
	results := testFetcher(t, s).GetAll(context.Background(), urls)
	if len(results) != MaxSources+1 {
		t.Fatalf("len=%d", len(results))
	}
	for i := 0; i < MaxSources; i++ {
		if results[i].Err != nil {
			t.Fatalf("url %d: %v", i, results[i].Err)
		}
	}
	if results[MaxSources].Err == nil {
		t.Fatal("expected cap error on 9th URL")
	}
}

type recordingHandler struct {
	mu      sync.Mutex
	records []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteByte(' ')
		b.WriteString(a.String())
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, b.String())
	h.mu.Unlock()
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func TestGetAllLogsHostNotURL(t *testing.T) {
	secret := "token=super-secret-query"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ics")
	}))
	defer s.Close()

	rec := &recordingHandler{}
	f := testFetcher(t, s)
	f.Logger = slog.New(rec)

	raw := s.URL + "/calendar/ical/user%40gmail.com/private/" + secret
	results := f.GetAll(context.Background(), []string{raw})
	if results[0].Err != nil {
		t.Fatal(results[0].Err)
	}

	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	joined := strings.Join(rec.records, "\n")
	rec.mu.Unlock()
	if !strings.Contains(joined, u.Hostname()) {
		t.Fatalf("expected host in logs, got %q", joined)
	}
	if !strings.Contains(joined, "200") {
		t.Fatalf("expected status in logs, got %q", joined)
	}
	if strings.Contains(joined, secret) || strings.Contains(joined, raw) || strings.Contains(joined, "/calendar/") {
		t.Fatalf("log leaked URL or secret: %q", joined)
	}
}

func TestGetAllErrorLogOmitsURL(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
	}))
	defer s.Close()

	rec := &recordingHandler{}
	f := testFetcher(t, s)
	f.Timeout = 30 * time.Millisecond
	f.Logger = slog.New(rec)

	secret := "q=ics-secret-token"
	raw := s.URL + "/feed.ics?" + secret
	_ = f.GetAll(context.Background(), []string{raw})
	rec.mu.Lock()
	joined := strings.Join(rec.records, "\n")
	rec.mu.Unlock()
	if strings.Contains(joined, secret) || strings.Contains(joined, "http://") || strings.Contains(joined, raw) {
		t.Fatalf("error log leaked URL: %q", joined)
	}
}

func TestValidateURLWrapsSSRFRules(t *testing.T) {
	ctx := context.Background()
	if err := ValidateURL(ctx, "https://localhost/cal.ics"); err == nil {
		t.Fatal("expected reject localhost")
	}
	if err := ValidateURL(ctx, "file:///etc/passwd"); err == nil {
		t.Fatal("expected reject file:")
	}
	if err := ValidateURL(ctx, "http://127.0.0.1/cal.ics"); err == nil {
		t.Fatal("expected reject loopback")
	}
}
