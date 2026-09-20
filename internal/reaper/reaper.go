// Package reaper deletes feeds whose ICS has not been fetched within retention.
package reaper

import (
	"context"
	"log/slog"
	"time"
)

const DefaultRetention = 14 * 24 * time.Hour

const defaultEvery = time.Hour

// StaleDeleter is store.DeleteStaleFeeds. The reaper must not issue its own SQL.
type StaleDeleter interface {
	DeleteStaleFeeds(ctx context.Context, now time.Time, retention time.Duration) (int64, error)
}

// Worker ticks and deletes stale feeds. Now is injected so tests can freeze time.
type Worker struct {
	Store     StaleDeleter
	Now       func() time.Time
	Every     time.Duration
	Retention time.Duration
	Logger    *slog.Logger
}

func (w Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w Worker) every() time.Duration {
	if w.Every > 0 {
		return w.Every
	}
	return defaultEvery
}

func (w Worker) retention() time.Duration {
	if w.Retention > 0 {
		return w.Retention
	}
	return DefaultRetention
}

func (w Worker) log() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

// RunOnce calls DeleteStaleFeeds once and logs the deleted count (not ids).
func (w Worker) RunOnce(ctx context.Context) error {
	if w.Store == nil {
		return nil
	}
	n, err := w.Store.DeleteStaleFeeds(ctx, w.now(), w.retention())
	if err != nil {
		w.log().Error("reaper", "err", err)
		return err
	}
	w.log().Info("reaper", "deleted", n)
	return nil
}

// Loop runs RunOnce immediately, then on each tick, until ctx is cancelled.
func (w Worker) Loop(ctx context.Context) {
	if w.Store == nil {
		return
	}
	_ = w.RunOnce(ctx)
	t := time.NewTicker(w.every())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = w.RunOnce(ctx)
		}
	}
}

// Start runs Loop in a new goroutine. Safe when Store is nil (no-op).
func Start(ctx context.Context, w Worker) {
	go w.Loop(ctx)
}
