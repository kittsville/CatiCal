package store_test

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"sci1.uk/catical/internal/store"
	"sci1.uk/catical/internal/tokens"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is required for store integration tests (see README)")
	}
	ctx := context.Background()
	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := s.Truncate(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

func tokenPair(t *testing.T) (salt, secret, hash []byte) {
	t.Helper()
	salt, secret, hash, err := tokens.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return salt, secret, hash
}

func TestMigrateApplyTwice(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if _, err := s.CountSources(ctx); err != nil {
		t.Fatalf("schema unusable after remigrate: %v", err)
	}
}

func TestInsertAndGetFeed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	feedSalt, feedSecret, feedHash := tokenPair(t)
	manageSalt, manageSecret, manageHash := tokenPair(t)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	created, err := s.CreateFeed(ctx, store.CreateFeedParams{
		Name:            "Work+Home",
		PrefixSummaries: true,
		FeedTokenSalt:   feedSalt,
		FeedTokenHash:   feedHash,
		ManageTokenSalt: manageSalt,
		ManageTokenHash: manageHash,
		LastRequestAt:   now,
		Sources: []store.CreateSource{
			{URL: "https://example.com/a.ics", Label: "A", Position: 0},
			{URL: "https://example.com/b.ics", Label: "B", Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == uuid.Nil {
		t.Fatal("expected feed id")
	}
	if !tokens.Verify(created.FeedTokenSalt, created.FeedTokenHash, feedSecret) {
		t.Fatal("stored feed token should verify")
	}
	if !tokens.Verify(created.ManageTokenSalt, created.ManageTokenHash, manageSecret) {
		t.Fatal("stored manage token should verify")
	}
	if tokens.Verify(created.FeedTokenSalt, created.FeedTokenHash, manageSecret) {
		t.Fatal("wrong secret must not verify")
	}

	got, err := s.GetFeed(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Work+Home" || !got.PrefixSummaries {
		t.Fatalf("feed fields: %+v", got)
	}
	if !got.LastRequestAt.Equal(now) {
		t.Fatalf("last_request_at: got %v want %v", got.LastRequestAt, now)
	}
	if len(got.Sources) != 2 {
		t.Fatalf("sources: %d", len(got.Sources))
	}
	if got.Sources[0].URL != "https://example.com/a.ics" || got.Sources[1].Label != "B" {
		t.Fatalf("sources: %+v", got.Sources)
	}
	if got.MergedICS != nil || got.MergedAt != nil {
		t.Fatalf("merged should be empty: ics=%v at=%v", got.MergedICS, got.MergedAt)
	}

	if bytes.Equal(got.FeedTokenHash, feedSecret) || bytes.Equal(got.FeedTokenSalt, feedSecret) {
		t.Fatal("must not store plaintext feed secret")
	}
	if bytes.Equal(got.ManageTokenHash, manageSecret) || bytes.Equal(got.ManageTokenSalt, manageSecret) {
		t.Fatal("must not store plaintext manage secret")
	}
}

func TestUpdateMerged(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	feed := mustCreate(t, s, time.Now().UTC())

	mergedAt := time.Date(2026, 9, 20, 13, 0, 0, 0, time.UTC)
	body := []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")
	if err := s.UpdateMerged(ctx, feed.ID, body, mergedAt); err != nil {
		t.Fatalf("update merged: %v", err)
	}

	got, err := s.GetFeed(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.MergedICS) != string(body) {
		t.Fatalf("merged body: %q", got.MergedICS)
	}
	if got.MergedAt == nil || !got.MergedAt.Equal(mergedAt) {
		t.Fatalf("merged_at: %v", got.MergedAt)
	}
}

func TestUpdateSourceFetch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	feed := mustCreate(t, s, time.Now().UTC())

	at := time.Date(2026, 9, 20, 14, 0, 0, 0, time.UTC)
	body := []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")
	src := feed.Sources[0]
	src.LastICS = body
	src.LastSuccessAt = &at
	src.LastAttemptAt = &at
	src.LastError = nil
	if err := s.UpdateSourceFetch(ctx, src); err != nil {
		t.Fatalf("update source: %v", err)
	}

	got, err := s.GetFeed(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Sources[0].LastICS) != string(body) {
		t.Fatalf("last_ics: %q", got.Sources[0].LastICS)
	}
	if got.Sources[0].LastSuccessAt == nil || !got.Sources[0].LastSuccessAt.Equal(at) {
		t.Fatalf("last_success_at: %v", got.Sources[0].LastSuccessAt)
	}

	msg := "http status 500"
	src.LastError = &msg
	if err := s.UpdateSourceFetch(ctx, src); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetFeed(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sources[0].LastError == nil || *got.Sources[0].LastError != msg {
		t.Fatalf("last_error: %v", got.Sources[0].LastError)
	}
}

func TestTouchLastRequestIsExplicit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	createdAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	feed := mustCreate(t, s, createdAt)

	if err := s.UpdateMerged(ctx, feed.ID, []byte("x"), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetFeed(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastRequestAt.Equal(createdAt) {
		t.Fatalf("UpdateMerged must not touch last_request_at: got %v", got.LastRequestAt)
	}

	touched := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)
	if err := s.TouchLastRequest(ctx, feed.ID, touched); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetFeed(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastRequestAt.Equal(touched) {
		t.Fatalf("touch: got %v want %v", got.LastRequestAt, touched)
	}
}

func TestDeleteFeedRemovesSources(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	feed := mustCreate(t, s, time.Now().UTC())

	if err := s.DeleteFeed(ctx, feed.ID); err != nil {
		t.Fatal(err)
	}
	_, err := s.GetFeed(ctx, feed.ID)
	if err != store.ErrNotFound {
		t.Fatalf("get after delete: %v", err)
	}
	n, err := s.CountSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("sources leftover: %d", n)
	}
}

func TestDeleteStaleFeeds(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	stale := mustCreate(t, s, now.Add(-15*24*time.Hour))
	fresh := mustCreate(t, s, now.Add(-13*24*time.Hour))

	deleted, err := s.DeleteStaleFeeds(ctx, now, 14*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted count: %d", deleted)
	}

	if _, err := s.GetFeed(ctx, stale.ID); err != store.ErrNotFound {
		t.Fatalf("stale should be gone: %v", err)
	}
	if _, err := s.GetFeed(ctx, fresh.ID); err != nil {
		t.Fatalf("fresh should remain: %v", err)
	}
}

func TestUpdateFeedReplacesSourcesAndClearsMerge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	feed := mustCreate(t, s, time.Now().UTC())
	if err := s.UpdateMerged(ctx, feed.ID, []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	err := s.UpdateFeed(ctx, feed.ID, store.UpdateFeedParams{
		Name:            "Renamed",
		PrefixSummaries: true,
		Sources: []store.CreateSource{
			{URL: "https://example.com/new.ics", Label: "N", Position: 0},
			{URL: "https://example.com/two.ics", Label: "T", Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := s.GetFeed(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Renamed" || !got.PrefixSummaries {
		t.Fatalf("fields: %+v", got)
	}
	if len(got.Sources) != 2 || got.Sources[0].URL != "https://example.com/new.ics" || got.Sources[1].Label != "T" {
		t.Fatalf("sources: %+v", got.Sources)
	}
	if got.MergedICS != nil || got.MergedAt != nil {
		t.Fatalf("merged cache should be cleared: ics=%v at=%v", got.MergedICS, got.MergedAt)
	}

	if err := s.UpdateFeed(ctx, uuid.New(), store.UpdateFeedParams{Name: "x"}); err != store.ErrNotFound {
		t.Fatalf("missing id: %v", err)
	}
}

func TestRotateTokens(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	feedSalt, feedSecret, feedHash := tokenPair(t)
	manageSalt, manageSecret, manageHash := tokenPair(t)
	feed, err := s.CreateFeed(ctx, store.CreateFeedParams{
		Name:            "rotate",
		FeedTokenSalt:   feedSalt,
		FeedTokenHash:   feedHash,
		ManageTokenSalt: manageSalt,
		ManageTokenHash: manageHash,
		LastRequestAt:   time.Now().UTC(),
		Sources:         []store.CreateSource{{URL: "https://example.com/a.ics", Position: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}

	newFeedSalt, newFeedSecret, newFeedHash := tokenPair(t)
	if err := s.RotateFeedToken(ctx, feed.ID, newFeedSalt, newFeedHash); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetFeed(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.Verify(got.FeedTokenSalt, got.FeedTokenHash, feedSecret) {
		t.Fatal("old feed secret must not verify")
	}
	if !tokens.Verify(got.FeedTokenSalt, got.FeedTokenHash, newFeedSecret) {
		t.Fatal("new feed secret should verify")
	}
	if !tokens.Verify(got.ManageTokenSalt, got.ManageTokenHash, manageSecret) {
		t.Fatal("manage token must be unchanged after feed rotate")
	}

	if err := s.RotateFeedToken(ctx, uuid.New(), newFeedSalt, newFeedHash); err != store.ErrNotFound {
		t.Fatalf("missing rotate feed: %v", err)
	}
}

func mustCreate(t *testing.T, s *store.Store, lastRequest time.Time) store.Feed {
	t.Helper()
	fs, _, fh := tokenPair(t)
	ms, _, mh := tokenPair(t)
	feed, err := s.CreateFeed(context.Background(), store.CreateFeedParams{
		Name:            t.Name(),
		FeedTokenSalt:   fs,
		FeedTokenHash:   fh,
		ManageTokenSalt: ms,
		ManageTokenHash: mh,
		LastRequestAt:   lastRequest,
		Sources: []store.CreateSource{
			{URL: "https://example.com/" + t.Name() + ".ics", Position: 0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return feed
}
