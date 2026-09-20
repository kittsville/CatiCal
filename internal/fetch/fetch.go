// Package fetch downloads ICS documents over HTTP(S) with SSRF protections.
//
// GetAll fetches URLs in parallel. Logs include host and status only — never
// the raw URL (query strings on calendar feeds are secrets).
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// MaxSources is the maximum number of origin URLs fetched in one GetAll.
	MaxSources = 8
	// DefaultMaxBody is 2 MiB.
	DefaultMaxBody = 2 << 20
	// DefaultTimeout is the per-request client timeout.
	DefaultTimeout = 5 * time.Second
	// DefaultMaxConcurrent is the process-wide cap on in-flight origin GETs.
	DefaultMaxConcurrent = 32
	maxRedirects         = 10
	httpsPort            = "443"
)

// Result is the outcome of one URL in a GetAll call, aligned by index.
type Result struct {
	Body   []byte
	Status int
	Err    error
}

// Fetcher performs bounded, SSRF-safe ICS GETs.
type Fetcher struct {
	Timeout       time.Duration
	MaxBody       int64
	MaxSources    int
	MaxConcurrent int
	Logger        *slog.Logger

	mu          sync.RWMutex
	permitHosts map[string]struct{} // exact host:port, for httptest

	lookupIPAddr func(ctx context.Context, host string) ([]net.IPAddr, error)

	semOnce sync.Once
	sem     chan struct{}
}

// New returns a Fetcher with production defaults (no loopback permit).
func New() *Fetcher {
	return &Fetcher{
		Timeout:       DefaultTimeout,
		MaxBody:       DefaultMaxBody,
		MaxSources:    MaxSources,
		MaxConcurrent: DefaultMaxConcurrent,
		Logger:        slog.Default(),
		permitHosts:   make(map[string]struct{}),
	}
}

var defaultFetcher = New()

// GetAll fetches urls in parallel using the default Fetcher (loopback blocked).
func GetAll(ctx context.Context, urls []string) []Result {
	return defaultFetcher.GetAll(ctx, urls)
}

// PermitHost allows fetches to a host[:port] that would otherwise fail SSRF
// checks (loopback httptest servers). Production callers must not use this.
func (f *Fetcher) PermitHost(hostport string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.permitHosts == nil {
		f.permitHosts = make(map[string]struct{})
	}
	// Exact host:port only. Permitting a bare hostname would allow any
	// port on loopback (SSRF via redirect to another local listener).
	f.permitHosts[strings.ToLower(hostport)] = struct{}{}
}

func (f *Fetcher) permitted(hostport string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	_, ok := f.permitHosts[strings.ToLower(hostport)]
	return ok
}

// GetAll GETs each URL concurrently. The returned slice has the same length
// as urls. URLs beyond MaxSources are not fetched and have a non-nil Err.
func (f *Fetcher) GetAll(ctx context.Context, urls []string) []Result {
	out := make([]Result, len(urls))
	max := f.MaxSources
	if max <= 0 {
		max = MaxSources
	}
	var wg sync.WaitGroup
	for i, raw := range urls {
		if i >= max {
			out[i].Err = fmt.Errorf("source cap %d exceeded", max)
			continue
		}
		wg.Add(1)
		go func(i int, raw string) {
			defer wg.Done()
			out[i] = f.getOne(ctx, raw)
		}(i, raw)
	}
	wg.Wait()
	return out
}

// ValidateURL reports whether raw is an allowed origin URL (https to a
// public host on port 443). It wraps the same SSRF rules as GetAll.
func ValidateURL(ctx context.Context, raw string) error {
	return defaultFetcher.ValidateURL(ctx, raw)
}

// ValidateURL reports whether raw is allowed for this Fetcher (including
// PermitHost exceptions).
func (f *Fetcher) ValidateURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	return f.validateURL(ctx, u)
}

func (f *Fetcher) lookup(ctx context.Context, host string) ([]net.IPAddr, error) {
	if f.lookupIPAddr != nil {
		return f.lookupIPAddr(ctx, host)
	}
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

func (f *Fetcher) acquire(ctx context.Context) error {
	f.semOnce.Do(func() {
		n := f.MaxConcurrent
		if n <= 0 {
			n = DefaultMaxConcurrent
		}
		f.sem = make(chan struct{}, n)
	})
	select {
	case f.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *Fetcher) release() {
	<-f.sem
}

func (f *Fetcher) getOne(ctx context.Context, raw string) Result {
	u, err := url.Parse(raw)
	if err != nil {
		return Result{Err: err}
	}
	if err := f.validateURL(ctx, u); err != nil {
		return Result{Err: err}
	}
	if err := f.acquire(ctx); err != nil {
		return Result{Err: err}
	}
	defer f.release()

	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	dialer := &net.Dialer{Timeout: timeout}
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           f.dialContext(dialer),
			DisableKeepAlives:     true,
			TLSHandshakeTimeout:   timeout,
			ResponseHeaderTimeout: timeout,
			ForceAttemptHTTP2:     false,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			return f.validateURL(req.Context(), req.URL)
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Result{Err: err}
	}
	req.Header.Set("User-Agent", "Catical/1.0")

	resp, err := client.Do(req)
	if err != nil {
		f.logHost(u, 0, "error")
		return Result{Err: err}
	}
	defer resp.Body.Close()

	maxBody := f.MaxBody
	if maxBody <= 0 {
		maxBody = DefaultMaxBody
	}
	limited := io.LimitReader(resp.Body, maxBody+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		f.logHost(u, resp.StatusCode, "read_error")
		return Result{Status: resp.StatusCode, Err: err}
	}
	if int64(len(body)) > maxBody {
		err := fmt.Errorf("response body exceeds %d bytes", maxBody)
		f.logHost(u, resp.StatusCode, "body_cap")
		return Result{Status: resp.StatusCode, Err: err}
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		f.logHost(u, resp.StatusCode, "status")
		return Result{Status: resp.StatusCode, Err: fmt.Errorf("unexpected status %d", resp.StatusCode)}
	}
	f.logHost(u, resp.StatusCode, "ok")
	return Result{Body: body, Status: resp.StatusCode}
}

func (f *Fetcher) dialContext(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if f.permitted(addr) {
			return d.DialContext(ctx, network, addr)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if port != httpsPort {
			return nil, fmt.Errorf("port %s not allowed", port)
		}
		ips, err := f.resolvePublic(ctx, host)
		if err != nil {
			return nil, err
		}
		var last error
		for _, ipa := range ips {
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
			if err != nil {
				last = err
				continue
			}
			tcp, ok := conn.RemoteAddr().(*net.TCPAddr)
			if !ok || !publicIP(tcp.IP) {
				_ = conn.Close()
				last = fmt.Errorf("connected address not allowed")
				continue
			}
			return conn, nil
		}
		if last == nil {
			last = errors.New("no allowed addresses")
		}
		return nil, last
	}
}

func (f *Fetcher) resolvePublic(ctx context.Context, host string) ([]net.IPAddr, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !publicIP(ip) {
			return nil, fmt.Errorf("address %s not allowed", host)
		}
		return []net.IPAddr{{IP: ip}}, nil
	}
	ips, err := f.lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve host: %w", err)
	}
	var out []net.IPAddr
	for _, addr := range ips {
		if publicIP(addr.IP) {
			out = append(out, addr)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("host %q resolved to no public addresses", host)
	}
	return out, nil
}

func (f *Fetcher) logHost(u *url.URL, status int, result string) {
	log := f.Logger
	if log == nil {
		log = slog.Default()
	}
	// Host + status + coarse result only. Never log the URL or net/http
	// error strings (they embed the request URL, including query secrets).
	log.Info("ics fetch", "host", u.Hostname(), "status", status, "result", result)
}

func (f *Fetcher) validateURL(ctx context.Context, u *url.URL) error {
	if u == nil {
		return errors.New("nil url")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("scheme %q not allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("missing host")
	}
	if blockedHost(host) {
		return fmt.Errorf("host %q not allowed", host)
	}

	port := u.Port()
	hostport := host
	if port != "" {
		hostport = net.JoinHostPort(host, port)
	} else if u.Host != "" {
		hostport = u.Host
	}
	if f.permitted(hostport) {
		return nil
	}

	if scheme != "https" {
		return fmt.Errorf("scheme %q not allowed", u.Scheme)
	}
	if port != "" && port != httpsPort {
		return fmt.Errorf("port %s not allowed", port)
	}

	ip := net.ParseIP(host)
	if ip != nil {
		if !publicIP(ip) {
			return fmt.Errorf("address %s not allowed", host)
		}
		return nil
	}

	ips, err := f.lookup(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve host: %w", err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("host %q resolved to no addresses", host)
	}
	for _, addr := range ips {
		if !publicIP(addr.IP) {
			return fmt.Errorf("host %q resolved to a non-public address", host)
		}
	}
	return nil
}

func blockedHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimSuffix(h, ".")
	switch h {
	case "localhost", "metadata.google.internal", "metadata":
		return true
	}
	return false
}

func publicIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() ||
		ip.IsInterfaceLocalMulticast() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		// Cloud metadata / link-local (also covered by IsLinkLocalUnicast).
		if ip4[0] == 169 && ip4[1] == 254 {
			return false
		}
		// CGNAT
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return false
		}
		return true
	}
	// Unique local IPv6 fc00::/7
	if len(ip) == net.IPv6len && (ip[0]&0xfe) == 0xfc {
		return false
	}
	return true
}
