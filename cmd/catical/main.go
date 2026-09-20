package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"sci1.uk/catical/internal/fetch"
	httpserver "sci1.uk/catical/internal/http"
	"sci1.uk/catical/internal/reaper"
	"sci1.uk/catical/internal/refresh"
	"sci1.uk/catical/internal/store"
	"sci1.uk/catical/internal/version"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLogLevel(os.Getenv("LOG_LEVEL")),
	}))
	slog.SetDefault(logger)

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		logger.Error("DATABASE_URL is required")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		logger.Error("postgres", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		logger.Error("migrate", "err", err)
		os.Exit(1)
	}

	fetcher := fetch.New()
	fetcher.Logger = logger
	refresher := refresh.New(st, fetcher, nil)

	reaper.Start(ctx, reaper.Worker{Store: st, Logger: logger})

	h := httpserver.New(httpserver.Config{
		Store:              st,
		Admin:              st,
		Refresh:            refresher,
		Fetch:              fetcher,
		BaseURL:            os.Getenv("BASE_URL"),
		Logger:             logger,
		TurnstileSecret:    os.Getenv("TURNSTILE_SECRET"),
		TurnstileSiteKey:   os.Getenv("TURNSTILE_SITE_KEY"),
		TurnstileHostnames: os.Getenv("TURNSTILE_HOSTNAMES"),
		Commit:             version.Resolve(os.Getenv("SOURCE_COMMIT")),
	})

	srv := &http.Server{Addr: ":8080", Handler: h}
	go func() {
		logger.Info("listen", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
