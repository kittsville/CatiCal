package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"sci1.uk/catical/internal/store"
	"sci1.uk/catical/internal/tokens"
)

const defaultBaseURL = "https://catical.sci1.uk"

// FeedLookup is the persistence the ICS handler needs.
type FeedLookup interface {
	GetFeed(ctx context.Context, id uuid.UUID) (store.Feed, error)
	TouchLastRequest(ctx context.Context, id uuid.UUID, at time.Time) error
}

// Admin is create/manage persistence. Separate from FeedLookup so ICS tests
// can keep a smaller fake.
type Admin interface {
	CreateFeed(ctx context.Context, p store.CreateFeedParams) (store.Feed, error)
	GetFeed(ctx context.Context, id uuid.UUID) (store.Feed, error)
	UpdateFeed(ctx context.Context, id uuid.UUID, p store.UpdateFeedParams) error
	DeleteFeed(ctx context.Context, id uuid.UUID) error
	RotateFeedToken(ctx context.Context, id uuid.UUID, salt, hash []byte) error
	RotateManageToken(ctx context.Context, id uuid.UUID, salt, hash []byte) error
}

// Refresher returns a merged ICS body for a feed id.
type Refresher interface {
	Refresh(ctx context.Context, id uuid.UUID) ([]byte, error)
}

// Config wires ICS GET and HTML admin to store + refresh.
type Config struct {
	Store     FeedLookup
	Admin     Admin
	Refresh   Refresher
	BaseURL   string
	Now       func() time.Time
	Logger    *slog.Logger
	POSTLimit int // per IP per minute; 0 uses defaultPOSTLimit

	// TurnstileSecret, when set, requires a Cloudflare Turnstile token on
	// create and manage POSTs. Unset disables Turnstile (local/dev). ICS GET
	// is never gated. TurnstileVerifier is optional; production uses siteverify.
	TurnstileSecret   string
	TurnstileSiteKey  string
	TurnstileVerifier TurnstileVerifier
}

func (c Config) baseURL() string {
	u := strings.TrimRight(c.BaseURL, "/")
	if u == "" {
		return defaultBaseURL
	}
	return u
}

// New returns a mux serving /healthz, HTML create/manage, and GET /c/{id}/{secret}.
func New(cfg Config) http.Handler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	createLim := newIPLimiter(cfg.POSTLimit, 0, cfg.Now)
	manageLim := newIPLimiter(cfg.POSTLimit, 0, cfg.Now)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", HealthHandler)
	mux.HandleFunc("GET /robots.txt", serveRobots)
	mux.HandleFunc("GET /c/{id}/{secret}", func(w http.ResponseWriter, r *http.Request) {
		serveICS(w, r, cfg)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		serveCreateForm(w, cfg, "", 0)
	})
	mux.HandleFunc("POST /{$}", func(w http.ResponseWriter, r *http.Request) {
		if !createLim.allow(peerIP(r)) {
			tooMany(w)
			return
		}
		serveCreate(w, r, cfg)
	})
	mux.HandleFunc("GET /m/{id}/{secret}", func(w http.ResponseWriter, r *http.Request) {
		serveManageGET(w, r, cfg)
	})
	mux.HandleFunc("POST /m/{id}/{secret}", func(w http.ResponseWriter, r *http.Request) {
		if !manageLim.allow(peerIP(r)) {
			tooMany(w)
			return
		}
		serveManagePOST(w, r, cfg)
	})
	return redactLog(cfg.Logger, mux)
}

func serveICS(w http.ResponseWriter, r *http.Request, cfg Config) {
	if cfg.Store == nil || cfg.Refresh == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	id, secret, err := parseIDAndSecret(r)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	feed, err := cfg.Store.GetFeed(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	if !tokens.Verify(feed.FeedTokenSalt, feed.FeedTokenHash, secret) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	body, err := cfg.Refresh.Refresh(r.Context(), id)
	if err != nil || len(body) == 0 {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("calendar unavailable\n"))
		return
	}

	if err := cfg.Store.TouchLastRequest(r.Context(), id, cfg.Now()); err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("calendar unavailable\n"))
		return
	}

	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

func redactLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Info("http",
			"method", r.Method,
			"path", redactPath(r.URL.Path),
			"status", rec.code,
		)
	})
}

func redactPath(path string) string {
	parts := strings.Split(path, "/")
	// /c/{id}/{secret}
	if len(parts) >= 4 && (parts[1] == "c" || parts[1] == "m") {
		parts[3] = "[redacted]"
		return strings.Join(parts, "/")
	}
	return path
}
