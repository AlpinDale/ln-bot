// Package announcer selects releases whose day has arrived and posts
// them to the alert channel, marking each as alerted so it never
// double-posts.
package announcer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/store"
)

// Poster delivers a single release alert. Implemented by the Discord
// bot layer; faked in tests.
type Poster interface {
	PostRelease(ctx context.Context, r model.Release) error
}

// Announcer runs the release-day announcement pass.
type Announcer struct {
	store  *store.Store
	poster Poster
	loc    *time.Location
	log    *slog.Logger
	now    func() time.Time // injectable for tests
}

// New builds an Announcer. loc defines what "today" means.
func New(st *store.Store, poster Poster, loc *time.Location, log *slog.Logger) *Announcer {
	return &Announcer{
		store:  st,
		poster: poster,
		loc:    loc,
		log:    log,
		now:    time.Now,
	}
}

// Run posts all unalerted releases dated today (in the configured
// timezone). A release is marked alerted only after a successful post; a
// posting failure leaves it eligible for the next run. Returns the number
// of releases announced.
//
// Only today's releases are announced — never a backlog. A backfill or a
// scrape run on a later day must not retroactively post past releases.
func (a *Announcer) Run(ctx context.Context) (int, error) {
	today := model.DateOnly(a.now().In(a.loc))

	due, err := a.store.UnalertedInWindow(ctx, today, today)
	if err != nil {
		return 0, fmt.Errorf("select due releases: %w", err)
	}

	posted := 0
	for _, r := range due {
		if err := ctx.Err(); err != nil {
			return posted, err
		}
		if err := a.poster.PostRelease(ctx, r); err != nil {
			// Leave unalerted; next run retries within the window.
			a.log.Error("post release failed", "title", r.VolumeTitle, "err", err)
			continue
		}
		if err := a.store.MarkAlerted(ctx, r.ID, a.now()); err != nil {
			// Worst case here is a duplicate alert next run — log loudly.
			a.log.Error("mark alerted failed", "id", r.ID, "title", r.VolumeTitle, "err", err)
			continue
		}
		posted++
	}
	return posted, nil
}
