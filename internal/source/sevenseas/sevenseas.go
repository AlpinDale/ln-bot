// Package sevenseas is the source plugin for Seven Seas Entertainment
// (light novel imprint: Airship).
//
// The whole site sits behind SiteGround's anti-bot, which challenges
// datacenter IPs. The shared fetch client handles this via the
// Cloudflare URL Scanner when credentials are set (rendering the page
// from Cloudflare's infra), falling back to a browser-TLS handshake for
// residential IPs — both configured for sevenseasentertainment.com by
// default. Data comes from two server-rendered list pages:
//
//   - /release-dates/          upcoming releases (~6 months forward)
//   - /release-dates/archive/  recently shipped releases
//
// Each row carries date, title (linked), format and ISBN. Light novels
// are selected by the FORMAT column ("Light Novel"/"Novel"). Covers are
// not on the list pages and per-product enrichment would cost one fetch
// per title, so releases are stored without a cover image.
package sevenseas

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

const defaultBaseURL = "https://sevenseasentertainment.com"

func init() {
	source.Register(&ss{baseURL: defaultBaseURL})
}

type ss struct {
	baseURL string // overridable in tests
}

func (s *ss) Name() string      { return "sevenseas" }
func (s *ss) Publisher() string { return "Seven Seas Entertainment" }

func (s *ss) Fetch(ctx context.Context, client *fetch.Client, mode source.Mode) ([]model.Release, error) {
	// Both modes read the same two pages — Seven Seas exposes only a
	// ~1-year window and there is no deeper pagination on these pages.
	pages := []string{
		s.baseURL + "/release-dates/",
		s.baseURL + "/release-dates/archive/",
	}
	var out []model.Release
	for _, page := range pages {
		body, err := client.Get(ctx, page)
		if err != nil {
			return nil, fmt.Errorf("sevenseas: %w", err)
		}
		rels, err := parseTable(string(body))
		if err != nil {
			return nil, fmt.Errorf("sevenseas %s: %w", page, err)
		}
		out = append(out, rels...)
	}
	return out, nil
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func parseTable(html string) ([]model.Release, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, err
	}

	var out []model.Release
	doc.Find("tr").Each(func(_ int, tr *goquery.Selection) {
		cells := tr.Find("td")
		if cells.Length() < 3 {
			return
		}
		dateStr := strings.TrimSpace(cells.Eq(0).Text())
		if !dateRe.MatchString(dateStr) {
			return
		}
		link := cells.Eq(1).Find("a").First()
		title := strings.TrimSpace(link.Text())
		href, _ := link.Attr("href")
		rawFormat := strings.TrimSpace(cells.Eq(2).Text())
		isbn := ""
		if cells.Length() >= 4 {
			isbn = strings.TrimSpace(cells.Eq(3).Text())
		}

		format, ok := classify(rawFormat, title)
		if !ok || title == "" {
			return
		}
		date, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			return
		}

		rel := model.Release{
			SeriesTitle: seriesFromTitle(title),
			VolumeTitle: title,
			Format:      format,
			ReleaseDate: model.DateOnly(date),
			URL:         href,
		}
		_ = isbn // available if we later want ISBN-keyed enrichment
		out = append(out, rel)
	})
	return out, nil
}

// classify maps a Seven Seas format-column value to a release format,
// keeping only light novels. Manga, omnibus (predominantly manga) and
// other prose are excluded. Audiobooks are kept only when the title
// marks them as light-novel adaptations. The list page does not split
// print vs ebook for a novel (one row per street date), so those map to
// FormatUnknown.
func classify(rawFormat, title string) (string, bool) {
	switch rawFormat {
	case "Light Novel", "Novel":
		return model.FormatUnknown, true
	case "Audiobook":
		lt := strings.ToLower(title)
		if strings.Contains(lt, "(light novel)") || strings.Contains(lt, "(novel)") {
			return model.FormatAudio, true
		}
		return "", false
	default:
		return "", false
	}
}

var volSuffixRe = regexp.MustCompile(`(?i)\s*\((?:light novel|novel|manga|omnibus|audiobook)\)\s*Vol(?:ume)?\.?\s*\d+.*$`)
var fmtParenRe = regexp.MustCompile(`(?i)\s*\((?:light novel|novel|manga|omnibus|audiobook)\)\s*$`)

// seriesFromTitle turns "My Status... (Light Novel) Vol. 5" into
// "My Status...".
func seriesFromTitle(title string) string {
	s := volSuffixRe.ReplaceAllString(title, "")
	if s == title {
		// No volume marker; at least drop a trailing format parenthetical.
		s = fmtParenRe.ReplaceAllString(title, "")
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return title
	}
	return s
}
