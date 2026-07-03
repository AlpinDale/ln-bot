package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/alpindale/ln-bot/internal/model"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func rel(title string, releaseDate time.Time) model.Release {
	return model.Release{
		SourceKey:   "testsrc",
		Publisher:   "Test Press",
		SeriesTitle: "Some Series",
		VolumeTitle: title,
		Format:      model.FormatDigital,
		ReleaseDate: releaseDate,
		URL:         "https://example.com/v1",
	}
}

func TestUpsertInsertThenDedupe(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	inserted, err := s.UpsertRelease(ctx, rel("Some Series Vol. 1", date(2026, 7, 10)), now)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !inserted {
		t.Fatal("first upsert should insert")
	}

	// Same title with different punctuation/case must dedupe.
	inserted, err = s.UpsertRelease(ctx, rel("Some Series, Vol: 1!", date(2026, 7, 10)), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	if inserted {
		t.Fatal("normalized-equal title should update, not insert")
	}

	got, err := s.ReleasesBetween(ctx, date(2026, 7, 1), date(2026, 7, 31), "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 release, got %d", len(got))
	}
}

func TestUpsertDateSlipUpdatesRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	if _, err := s.UpsertRelease(ctx, rel("Vol 1", date(2026, 8, 1)), now); err != nil {
		t.Fatal(err)
	}
	// Publisher delays the release by a month.
	inserted, err := s.UpsertRelease(ctx, rel("Vol 1", date(2026, 9, 1)), now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("date slip must update existing row, not insert")
	}

	got, err := s.ReleasesBetween(ctx, date(2026, 8, 1), date(2026, 9, 30), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 release after date slip, got %d", len(got))
	}
	if !got[0].ReleaseDate.Equal(date(2026, 9, 1)) {
		t.Fatalf("release date not updated: %v", got[0].ReleaseDate)
	}
	if got[0].FirstSeenAt.After(now.Add(time.Minute)) {
		t.Fatalf("first_seen_at must be preserved, got %v", got[0].FirstSeenAt)
	}
}

func TestFormatsAreDistinctRows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	digital := rel("Vol 1", date(2026, 7, 10))
	physical := digital
	physical.Format = model.FormatPhysical
	physical.ReleaseDate = date(2026, 10, 20)

	for _, r := range []model.Release{digital, physical} {
		if ins, err := s.UpsertRelease(ctx, r, now); err != nil || !ins {
			t.Fatalf("upsert %s: inserted=%v err=%v", r.Format, ins, err)
		}
	}
	got, err := s.ReleasesBetween(ctx, date(2026, 1, 1), date(2026, 12, 31), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows (digital+physical), got %d", len(got))
	}
}

func TestAlertWindowAndMarkAlerted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	inWindow := rel("Out Today", date(2026, 7, 3))
	tooOld := rel("Long Past", date(2026, 6, 1))
	future := rel("Not Yet", date(2026, 7, 20))
	for _, r := range []model.Release{inWindow, tooOld, future} {
		if _, err := s.UpsertRelease(ctx, r, now); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.UnalertedInWindow(ctx, date(2026, 6, 30), date(2026, 7, 3))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].VolumeTitle != "Out Today" {
		t.Fatalf("window selection wrong: %+v", got)
	}

	if err := s.MarkAlerted(ctx, got[0].ID, now); err != nil {
		t.Fatal(err)
	}
	got, err = s.UnalertedInWindow(ctx, date(2026, 6, 30), date(2026, 7, 3))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("alerted release must not reappear, got %+v", got)
	}
}

func TestPublisherFilter(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	a := rel("A Vol 1", date(2026, 7, 10))
	b := rel("B Vol 1", date(2026, 7, 11))
	b.Publisher = "Other House"
	for _, r := range []model.Release{a, b} {
		if _, err := s.UpsertRelease(ctx, r, now); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ReleasesBetween(ctx, date(2026, 7, 1), date(2026, 7, 31), "test press")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Publisher != "Test Press" {
		t.Fatalf("publisher filter wrong: %+v", got)
	}
}

func TestScrapeRunHistory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)

	runs := []ScrapeRun{
		{SourceKey: "src1", StartedAt: base, FinishedAt: base.Add(time.Minute), Status: "error", Error: "boom"},
		{SourceKey: "src1", StartedAt: base.Add(time.Hour), FinishedAt: base.Add(61 * time.Minute), Status: "ok", Count: 42},
		{SourceKey: "src2", StartedAt: base, FinishedAt: base.Add(time.Minute), Status: "ok", Count: 7},
	}
	for _, r := range runs {
		if err := s.RecordScrapeRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	last, err := s.LastRunPerSource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 2 {
		t.Fatalf("want 2 sources, got %d", len(last))
	}
	if last["src1"].Status != "ok" || last["src1"].Count != 42 {
		t.Fatalf("latest run for src1 wrong: %+v", last["src1"])
	}
}
