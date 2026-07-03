package scraper

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/source"
	"github.com/alpindale/ln-bot/internal/source/fetch"
	"github.com/alpindale/ln-bot/internal/store"
)

type fakeSource struct {
	name    string
	fetched *int
}

func (f *fakeSource) Name() string      { return f.name }
func (f *fakeSource) Publisher() string { return f.name }
func (f *fakeSource) Fetch(_ context.Context, _ *fetch.Client, _ source.Mode) ([]model.Release, error) {
	*f.fetched++
	return []model.Release{{
		VolumeTitle: f.name + " Vol. 1",
		Format:      model.FormatDigital,
		ReleaseDate: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}}, nil
}

func TestRunAllSourceFilter(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var aHits, bHits int
	srcs := []source.Source{
		&fakeSource{name: "alpha", fetched: &aHits},
		&fakeSource{name: "bravo", fetched: &bHits},
	}
	sc := New(st, fetch.New(fetch.Options{MinDelay: time.Millisecond}),
		func() []source.Source { return srcs }, slog.Default())
	ctx := context.Background()

	// Filter to one source: only it runs.
	res, err := sc.RunAll(ctx, source.ModeIncremental, []string{"bravo"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Sources != 1 || aHits != 0 || bHits != 1 {
		t.Fatalf("filter failed: sources=%d aHits=%d bHits=%d", res.Sources, aHits, bHits)
	}

	// No filter: every source runs.
	res, err = sc.RunAll(ctx, source.ModeIncremental, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sources != 2 || aHits != 1 || bHits != 2 {
		t.Fatalf("unfiltered failed: sources=%d aHits=%d bHits=%d", res.Sources, aHits, bHits)
	}
}
