// Package scraper orchestrates fetching from all enabled sources and
// persisting the results.
package scraper

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/source"
	"github.com/alpindale/ln-bot/internal/source/fetch"
	"github.com/alpindale/ln-bot/internal/store"
)

// Result summarizes one full scrape pass.
type Result struct {
	Sources  int
	Fetched  int // releases reported by sources
	New      int // rows newly inserted
	Failures int
}

// Scraper runs enabled sources sequentially and upserts their releases.
type Scraper struct {
	store   *store.Store
	client  *fetch.Client
	sources func() []source.Source
	log     *slog.Logger

	mu sync.Mutex // serializes runs (cron vs /scrape)
}

// New builds a Scraper. sources is called at run time so enablement
// reflects current config.
func New(st *store.Store, client *fetch.Client, sources func() []source.Source, log *slog.Logger) *Scraper {
	return &Scraper{store: st, client: client, sources: sources, log: log}
}

// RunAll fetches every enabled source in the given mode. Per-source
// failures are logged and recorded but do not abort the pass.
// Concurrent calls serialize.
func (s *Scraper) RunAll(ctx context.Context, mode source.Mode) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var res Result
	for _, src := range s.sources() {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Sources++
		s.log.Info("scraping source", "source", src.Name(), "mode", mode.String())
		run := store.ScrapeRun{SourceKey: src.Name(), StartedAt: time.Now()}
		releases, err := src.Fetch(ctx, s.client, mode)
		run.FinishedAt = time.Now()

		if err != nil {
			run.Status = "error"
			run.Error = err.Error()
			res.Failures++
			s.log.Error("source fetch failed", "source", src.Name(), "err", err)
		} else {
			run.Status = "ok"
			run.Count = len(releases)
			res.Fetched += len(releases)
			inserted := s.upsertAll(ctx, src, releases)
			res.New += inserted
			s.log.Info("source fetched", "source", src.Name(),
				"releases", len(releases), "new", inserted)
		}
		if rerr := s.store.RecordScrapeRun(ctx, run); rerr != nil {
			s.log.Error("record scrape run failed", "source", src.Name(), "err", rerr)
		}
	}
	return res, nil
}

func (s *Scraper) upsertAll(ctx context.Context, src source.Source, releases []model.Release) int {
	now := time.Now()
	inserted := 0
	for _, r := range releases {
		// Trust the registry over the plugin for the key fields.
		r.SourceKey = src.Name()
		if r.Publisher == "" {
			r.Publisher = src.Publisher()
		}
		if r.VolumeTitle == "" || r.ReleaseDate.IsZero() {
			s.log.Warn("skipping malformed release", "source", src.Name(), "release", r)
			continue
		}
		ins, err := s.store.UpsertRelease(ctx, r, now)
		if err != nil {
			s.log.Error("upsert failed", "source", src.Name(), "title", r.VolumeTitle, "err", err)
			continue
		}
		if ins {
			inserted++
		}
	}
	return inserted
}
