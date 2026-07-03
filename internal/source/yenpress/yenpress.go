// Package yenpress is the source plugin for Yen Press (Yen On imprint).
//
// The release calendar at yenpress.com/calendar is server-rendered and
// filterable via plain GET params: one request per (year, month) with
// imprint_id[]=2 returns every Yen On (light novel) release that month.
// Each item carries title, day+month, cover (with ISBN in the URL) and
// the product link; items are additionally tagged "Novels", which we
// assert as a second filter.
//
// The calendar does not distinguish digital from physical editions —
// one entry per street date — so releases are stored with format
// "unknown" rather than guessing (and rather than enriching via product
// pages, which would split the dedupe key across modes).
package yenpress

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/source"
	"github.com/alpindale/ln-bot/internal/source/fetch"
)

const (
	defaultBaseURL = "https://yenpress.com"
	yenOnImprintID = "2"

	// Incremental: previous month through 4 months ahead (covers the
	// announce lookback across month boundaries and the /upcoming 90d).
	incrementalBack    = 1
	incrementalForward = 4

	// Full: Yen On's earliest calendar entries are ~2010; catalog is
	// announced roughly 6 months out.
	fullStartYear   = 2010
	fullForwardMons = 7
)

func init() {
	source.Register(&yen{baseURL: defaultBaseURL})
}

type yen struct {
	baseURL string // overridable in tests
}

func (y *yen) Name() string      { return "yenpress" }
func (y *yen) Publisher() string { return "Yen Press" }

var months = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March,
	"apr": time.April, "may": time.May, "jun": time.June,
	"jul": time.July, "aug": time.August, "sep": time.September,
	"oct": time.October, "nov": time.November, "dec": time.December,
}

func (y *yen) Fetch(ctx context.Context, client *fetch.Client, mode source.Mode) ([]model.Release, error) {
	now := time.Now().UTC()
	first := now.AddDate(0, -incrementalBack, 0)
	last := now.AddDate(0, incrementalForward, 0)
	if mode == source.ModeFull {
		first = time.Date(fullStartYear, 1, 1, 0, 0, 0, 0, time.UTC)
		last = now.AddDate(0, fullForwardMons, 0)
	}

	var out []model.Release
	for cur := time.Date(first.Year(), first.Month(), 1, 0, 0, 0, 0, time.UTC); !cur.After(last); cur = cur.AddDate(0, 1, 0) {
		rels, err := y.fetchMonth(ctx, client, cur.Year(), cur.Month())
		if err != nil {
			return nil, err
		}
		out = append(out, rels...)
	}
	return out, nil
}

func (y *yen) fetchMonth(ctx context.Context, client *fetch.Client, year int, month time.Month) ([]model.Release, error) {
	q := url.Values{
		"year":         {strconv.Itoa(year)},
		"month":        {strconv.Itoa(int(month))},
		"imprint_id[]": {yenOnImprintID},
	}
	body, err := client.Get(ctx, y.baseURL+"/calendar?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("yenpress %d-%02d: %w", year, month, err)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("yenpress %d-%02d: parse: %w", year, month, err)
	}

	var out []model.Release
	doc.Find("div.released-box.book-box").Each(func(_ int, box *goquery.Selection) {
		// Belt and braces: the imprint filter should already exclude
		// manga/audio, but assert the category tag anyway.
		if !strings.EqualFold(strings.TrimSpace(box.Find("span.white-label").Text()), "novels") {
			return
		}

		title := strings.TrimSpace(box.Find("h3.heading").Text())
		if title == "" {
			return
		}

		// p.label-date reads like "14 Jul".
		fields := strings.Fields(box.Find("p.label-date").Text())
		if len(fields) < 2 {
			return
		}
		day, err := strconv.Atoi(fields[0])
		if err != nil {
			return
		}
		mon, ok := months[strings.ToLower(fields[1])]
		if !ok || mon != month {
			return
		}

		rel := model.Release{
			SeriesTitle: seriesFromTitle(title),
			VolumeTitle: title,
			Format:      model.FormatUnknown,
			ReleaseDate: time.Date(year, mon, day, 0, 0, 0, 0, time.UTC),
			CoverURL:    box.Find("img.genre-col-img").AttrOr("src", ""),
		}
		if href, ok := box.ParentsFiltered("a").First().Attr("href"); ok {
			rel.URL = y.baseURL + href
		}
		out = append(out, rel)
	})
	return out, nil
}

var (
	lnSuffixRe  = regexp.MustCompile(`(?i)\s*\((light\s+)?novel\)\s*$`)
	volSuffixRe = regexp.MustCompile(`(?i)[,:]?\s*Vol(?:ume)?\.?\s*\d+.*$`)
)

// seriesFromTitle recovers the series name from titles like
// "A Certain Magical Index NT, Vol. 6 (light novel)".
func seriesFromTitle(title string) string {
	s := lnSuffixRe.ReplaceAllString(title, "")
	s = strings.TrimSpace(volSuffixRe.ReplaceAllString(s, ""))
	if s == "" {
		return title
	}
	return s
}
