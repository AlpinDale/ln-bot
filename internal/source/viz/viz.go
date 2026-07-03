// Package viz is the source plugin for Viz Media.
//
// Viz's month calendar (/calendar/YYYY/MM) is server-rendered HTML; each
// release is an <article> whose category label distinguishes "Novel"
// from "Manga"/"Graphic Novel"/etc. Viz publishes only ~1-3 light
// novels a month. The calendar row lacks the exact day, so each novel's
// product page (robots-allowed) is fetched for its release date — a
// handful of extra requests at most.
//
// Politeness: viz.com's robots.txt sets Crawl-delay: 2. The shared
// fetch client enforces this via the host_delay_ms config default; do
// not bypass it.
package viz

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/source"
	"github.com/alpindale/ln-bot/internal/source/fetch"
)

const (
	defaultBaseURL = "https://www.viz.com"

	// Incremental: previous month through 3 months ahead.
	incrementalBack    = 1
	incrementalForward = 3

	// Full: Viz's LN line is tiny; calendar pages this old are still
	// served (empty months return a valid page with no articles).
	fullStartYear   = 2015
	fullForwardMons = 6
)

func init() {
	source.Register(&viz{baseURL: defaultBaseURL})
}

type viz struct {
	baseURL string // overridable in tests
}

func (v *viz) Name() string      { return "viz" }
func (v *viz) Publisher() string { return "VIZ Media" }

func (v *viz) Fetch(ctx context.Context, client *fetch.Client, mode source.Mode) ([]model.Release, error) {
	now := time.Now().UTC()
	first := now.AddDate(0, -incrementalBack, 0)
	last := now.AddDate(0, incrementalForward, 0)
	if mode == source.ModeFull {
		first = time.Date(fullStartYear, 1, 1, 0, 0, 0, 0, time.UTC)
		last = now.AddDate(0, fullForwardMons, 0)
	}

	var out []model.Release
	for cur := time.Date(first.Year(), first.Month(), 1, 0, 0, 0, 0, time.UTC); !cur.After(last); cur = cur.AddDate(0, 1, 0) {
		rels, err := v.fetchMonth(ctx, client, cur.Year(), int(cur.Month()))
		if err != nil {
			return nil, err
		}
		out = append(out, rels...)
	}
	return out, nil
}

func (v *viz) fetchMonth(ctx context.Context, client *fetch.Client, year, month int) ([]model.Release, error) {
	body, err := client.Get(ctx, fmt.Sprintf("%s/calendar/%d/%02d", v.baseURL, year, month))
	if err != nil {
		return nil, fmt.Errorf("viz %d-%02d: %w", year, month, err)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("viz %d-%02d: parse: %w", year, month, err)
	}

	var out []model.Release
	var ferr error
	doc.Find("article").EachWithBreak(func(_ int, art *goquery.Selection) bool {
		label := strings.TrimSpace(art.Find("a.color-mid-gray").First().Text())
		if label != "Novel" {
			return true // manga, graphic novel, art book, ...
		}
		link := art.Find("a.color-off-black").First()
		title := strings.TrimSpace(link.Text())
		href, _ := link.Attr("href")
		if title == "" || href == "" {
			return true
		}
		cover := art.Find("img").First().AttrOr("data-original", "")

		date, err := v.fetchReleaseDate(ctx, client, href)
		if err != nil {
			ferr = err
			return false
		}
		if date.IsZero() || date.Year() != year || int(date.Month()) != month {
			// No parseable date, or the product page disagrees with the
			// calendar month — trust the product page only when sane.
			if date.IsZero() {
				return true
			}
		}

		out = append(out, model.Release{
			SeriesTitle: seriesFromSlug(href),
			VolumeTitle: title,
			Format:      formatFromHref(href),
			ReleaseDate: model.DateOnly(date),
			URL:         v.baseURL + href,
			CoverURL:    cover,
		})
		return true
	})
	if ferr != nil {
		return nil, ferr
	}
	return out, nil
}

var releaseDateRe = regexp.MustCompile(`o_release-date[^>]*>\s*<strong>Release</strong>\s*([A-Za-z]+ \d{1,2}, \d{4})`)

func (v *viz) fetchReleaseDate(ctx context.Context, client *fetch.Client, href string) (time.Time, error) {
	body, err := client.Get(ctx, v.baseURL+href)
	if err != nil {
		return time.Time{}, fmt.Errorf("viz product %s: %w", href, err)
	}
	m := releaseDateRe.FindSubmatch(body)
	if m == nil {
		return time.Time{}, nil
	}
	date, err := time.Parse("January 2, 2006", string(m[1]))
	if err != nil {
		return time.Time{}, nil
	}
	return date, nil
}

// seriesFromSlug derives a series name from the product path:
// /manga-books/novel/sakamoto-days-novels/product/8913/paperback
// -> "Sakamoto Days".
func seriesFromSlug(href string) string {
	parts := strings.Split(strings.Trim(href, "/"), "/")
	if len(parts) < 3 {
		return ""
	}
	words := strings.Split(parts[2], "-")
	// Drop a trailing "novels"/"novel" marker.
	if n := len(words); n > 1 && (words[n-1] == "novels" || words[n-1] == "novel") {
		words = words[:n-1]
	}
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

func formatFromHref(href string) string {
	switch {
	case strings.HasSuffix(href, "/paperback"), strings.HasSuffix(href, "/hardcover"):
		return model.FormatPhysical
	case strings.HasSuffix(href, "/digital"):
		return model.FormatDigital
	default:
		return model.FormatUnknown
	}
}
