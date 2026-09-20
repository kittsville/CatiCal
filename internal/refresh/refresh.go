// Package refresh orchestrates cache policy, origin fetch, merge, and store.
package refresh

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"

	"sci1.uk/catical/internal/cachepolicy"
	"sci1.uk/catical/internal/fetch"
	"sci1.uk/catical/internal/icalmerge"
	"sci1.uk/catical/internal/store"
)

// ErrNoUsableSources is returned when no origin body can be merged and the
// merged cache cannot be served.
var ErrNoUsableSources = errors.New("refresh: no usable source calendars")

// Fetcher downloads origin ICS documents. Production uses *fetch.Fetcher.
type Fetcher interface {
	GetAll(ctx context.Context, urls []string) []fetch.Result
}

// FeedStore is the persistence the service needs. It does not include
// TouchLastRequest — HTTP owns GDPR timestamps.
type FeedStore interface {
	GetFeed(ctx context.Context, id uuid.UUID) (store.Feed, error)
	UpdateMerged(ctx context.Context, id uuid.UUID, ics []byte, mergedAt time.Time) error
	UpdateSourceFetch(ctx context.Context, src store.Source) error
}

var _ FeedStore = (*store.Store)(nil)

// Service composes policy, fetch, merge, and store. Max 8 sources.
type Service struct {
	Store FeedStore
	Fetch Fetcher
	Now   func() time.Time

	group singleflight.Group
}

// New returns a Service. now may be nil (uses time.Now).
func New(st FeedStore, f Fetcher, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{Store: st, Fetch: f, Now: now}
}

// Refresh returns a merged ICS body for feed id. Concurrent calls for the
// same id share one origin fetch burst.
func (s *Service) Refresh(ctx context.Context, id uuid.UUID) ([]byte, error) {
	v, err, _ := s.group.Do(id.String(), func() (any, error) {
		return s.refreshOnce(ctx, id)
	})
	if err != nil {
		return nil, err
	}
	body, _ := v.([]byte)
	return body, nil
}

func (s *Service) refreshOnce(ctx context.Context, id uuid.UUID) ([]byte, error) {
	feed, err := s.Store.GetFeed(ctx, id)
	if err != nil {
		return nil, err
	}
	now := s.Now()

	mergedAt := time.Time{}
	if feed.MergedAt != nil {
		mergedAt = *feed.MergedAt
	}
	hasMerged := len(feed.MergedICS) > 0
	decision := cachepolicy.Decide(mergedAt, now, hasMerged)
	if !decision.ShouldFetch() {
		return append([]byte(nil), feed.MergedICS...), nil
	}

	sources := feed.Sources
	if len(sources) > fetch.MaxSources {
		sources = sources[:fetch.MaxSources]
	}

	urls := make([]string, len(sources))
	for i, src := range sources {
		urls[i] = src.URL
	}
	results := s.Fetch.GetAll(ctx, urls)
	if len(results) < len(sources) {
		pad := make([]fetch.Result, len(sources)-len(results))
		for i := range pad {
			pad[i].Err = errors.New("missing fetch result")
		}
		results = append(results, pad...)
	}

	var named []icalmerge.NamedCalendar
	for i, src := range sources {
		res := results[i]
		updated := applyFetch(src, res, now)
		if err := s.Store.UpdateSourceFetch(ctx, updated); err != nil {
			return nil, err
		}
		body := usableSourceBody(updated, now)
		if len(body) == 0 {
			continue
		}
		prefix := ""
		if feed.PrefixSummaries {
			prefix = updated.Label
		}
		named = append(named, icalmerge.NamedCalendar{
			ID:            updated.ID.String(),
			SummaryPrefix: prefix,
			ICS:           body,
		})
	}

	if len(named) == 0 {
		if decision.ServeCacheOnError() && hasMerged {
			return append([]byte(nil), feed.MergedICS...), nil
		}
		return nil, ErrNoUsableSources
	}

	merged, err := icalmerge.Merge(feed.Name, named)
	if err != nil {
		if decision.ServeCacheOnError() && hasMerged {
			return append([]byte(nil), feed.MergedICS...), nil
		}
		return nil, err
	}
	if err := s.Store.UpdateMerged(ctx, feed.ID, merged, now); err != nil {
		return nil, err
	}
	return merged, nil
}

func applyFetch(src store.Source, res fetch.Result, now time.Time) store.Source {
	attempt := now
	src.LastAttemptAt = &attempt
	if res.Err == nil && len(res.Body) > 0 {
		src.LastICS = append([]byte(nil), res.Body...)
		success := now
		src.LastSuccessAt = &success
		src.LastError = nil
		return src
	}
	msg := fetchErrorMessage(res)
	src.LastError = &msg
	return src
}

func fetchErrorMessage(res fetch.Result) string {
	if res.Status != 0 {
		return fmt.Sprintf("http status %d", res.Status)
	}
	return "fetch error"
}

func usableSourceBody(src store.Source, now time.Time) []byte {
	successAt := time.Time{}
	if src.LastSuccessAt != nil {
		successAt = *src.LastSuccessAt
	}
	d := cachepolicy.Decide(successAt, now, len(src.LastICS) > 0)
	if d == cachepolicy.FetchFailOnError {
		return nil
	}
	return src.LastICS
}
