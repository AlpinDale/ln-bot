package bot

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alpindale/ln-bot/internal/model"
)

func TestPageIDRoundTrip(t *testing.T) {
	q := pageQuery{
		From:      time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		To:        time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
		Publisher: "Seven Seas",
	}
	id := encodePageID(q, 3)
	if len(id) > 100 {
		t.Fatalf("custom_id too long: %d", len(id))
	}
	got, ok := parsePageID(id)
	if !ok {
		t.Fatalf("parse failed for %q", id)
	}
	if !got.From.Equal(q.From) || !got.To.Equal(q.To) || got.Publisher != q.Publisher || got.Page != 3 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestPageIDSanitizesPublisher(t *testing.T) {
	q := pageQuery{
		From:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		To:        time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Publisher: "evil:pub:" + strings.Repeat("x", 200),
	}
	id := encodePageID(q, 0)
	if len(id) > 100 {
		t.Fatalf("custom_id too long: %d", len(id))
	}
	got, ok := parsePageID(id)
	if !ok {
		t.Fatal("parse failed")
	}
	if strings.Contains(got.Publisher[:7], ":") {
		t.Fatalf("separator not stripped: %q", got.Publisher)
	}
}

func TestParsePageIDRejectsGarbage(t *testing.T) {
	for _, id := range []string{
		"", "rel", "rel:indicator", "other:2026-01-01:2026-01-02:0:",
		"rel:notadate:2026-01-02:0:", "rel:2026-01-01:2026-01-02:-1:",
		"rel:2026-01-01:2026-01-02:NaN:",
	} {
		if _, ok := parsePageID(id); ok {
			t.Errorf("parsePageID(%q) accepted garbage", id)
		}
	}
}

func TestPaginateReleasesRespectsCaps(t *testing.T) {
	var releases []model.Release
	for i := 0; i < 47; i++ {
		releases = append(releases, model.Release{
			VolumeTitle: fmt.Sprintf("Series Vol. %d", i+1),
			Publisher:   "Test Press",
			Format:      model.FormatDigital,
			ReleaseDate: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			URL:         "https://example.com/v",
		})
	}
	pages := paginateReleases(releases)

	total := 0
	for pi, p := range pages {
		if len(p) > pageMaxLines {
			t.Fatalf("page %d has %d lines (cap %d)", pi, len(p), pageMaxLines)
		}
		size := 0
		for _, line := range p {
			size += len(line)
		}
		if size > pageCharBudget {
			t.Fatalf("page %d is %d chars (budget %d)", pi, size, pageCharBudget)
		}
		total += len(p)
	}
	if total != 47 {
		t.Fatalf("lines lost in pagination: %d/47", total)
	}
	// 47 releases at 15 lines/page = 4 pages.
	if len(pages) != 4 {
		t.Fatalf("want 4 pages, got %d", len(pages))
	}
}

func TestPaginateSingleOversizeLine(t *testing.T) {
	r := model.Release{
		VolumeTitle: strings.Repeat("long ", 100),
		Publisher:   "P", Format: model.FormatDigital,
		ReleaseDate: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}
	pages := paginateReleases([]model.Release{r, r})
	if len(pages) == 0 {
		t.Fatal("no pages")
	}
	total := 0
	for _, p := range pages {
		total += len(p)
	}
	if total != 2 {
		t.Fatalf("lines lost: %d/2", total)
	}
}
