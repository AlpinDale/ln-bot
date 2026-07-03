package announcer

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/store"
)

type fakePoster struct {
	posted []string
	fail   map[string]bool
}

func (f *fakePoster) PostRelease(_ context.Context, r model.Release) error {
	if f.fail[r.VolumeTitle] {
		return errors.New("discord down")
	}
	f.posted = append(f.posted, r.VolumeTitle)
	return nil
}

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func setup(t *testing.T) (*store.Store, *fakePoster, *Announcer) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := &fakePoster{fail: map[string]bool{}}
	a := New(st, p, time.UTC, 3, slog.Default())
	// Frozen "today": 2026-07-03.
	a.now = func() time.Time { return time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC) }
	return st, p, a
}

func add(t *testing.T, st *store.Store, title string, d time.Time) {
	t.Helper()
	_, err := st.UpsertRelease(context.Background(), model.Release{
		SourceKey:   "test",
		Publisher:   "Test Press",
		SeriesTitle: title,
		VolumeTitle: title,
		Format:      model.FormatDigital,
		ReleaseDate: d,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
}

func TestAnnouncesOnlyWindow(t *testing.T) {
	st, p, a := setup(t)
	add(t, st, "Today", date(2026, 7, 3))
	add(t, st, "WithinLookback", date(2026, 7, 1))
	add(t, st, "TooOld", date(2026, 6, 20))
	add(t, st, "Future", date(2026, 7, 10))

	n, err := a.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("want 2 announced, got %d (%v)", n, p.posted)
	}
	if len(p.posted) != 2 || p.posted[0] != "WithinLookback" || p.posted[1] != "Today" {
		t.Fatalf("posted wrong set/order: %v", p.posted)
	}
}

func TestNoDoublePost(t *testing.T) {
	st, p, a := setup(t)
	add(t, st, "Today", date(2026, 7, 3))

	if _, err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.posted) != 1 {
		t.Fatalf("release posted %d times, want 1", len(p.posted))
	}
}

func TestFailedPostRetriesNextRun(t *testing.T) {
	st, p, a := setup(t)
	add(t, st, "Flaky", date(2026, 7, 3))

	p.fail["Flaky"] = true
	n, err := a.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("failed post must not count as announced, got %d", n)
	}

	p.fail["Flaky"] = false
	n, err = a.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(p.posted) != 1 {
		t.Fatalf("release not retried after failure: n=%d posted=%v", n, p.posted)
	}
}
