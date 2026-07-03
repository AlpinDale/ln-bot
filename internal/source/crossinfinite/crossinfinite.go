// Package crossinfinite is the source plugin for Cross Infinite World.
//
// The site is hand-built static HTML. Its homepage news feed doubles as
// a dated release log: posts titled "New Release: <title>" (digital),
// "New Print Release: <title>" (physical) and "New Audiobook Release:
// <title>" (audio) are published on the release day itself, so the post
// date IS the release date. "New Volume:" posts are pre-release
// announcements without a firm date and are skipped, as are license
// news and other posts.
//
// Incremental mode parses the homepage (newest ~35 posts). Full mode
// additionally walks the news-N.html archive pages.
package crossinfinite

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"

	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/source"
	"github.com/alpindale/ln-bot/internal/source/fetch"
)

const (
	defaultBaseURL = "https://crossinfworld.com"
	// Safety cap when walking archive pages in full mode.
	maxArchivePages = 20
)

func init() {
	source.Register(&ciw{baseURL: defaultBaseURL})
}

type ciw struct {
	baseURL string // overridable in tests
}

func (c *ciw) Name() string      { return "crossinfinite" }
func (c *ciw) Publisher() string { return "Cross Infinite World" }

// The feed is rigid hand-written HTML (no CMS), so a regexp is the
// pragmatic parser. Each post looks like:
//
//	<li onclick="location.href='news-articles/New-Release-....html';">
//	  <img src="img/lncover/....jpg" ...>
//	  ... <h3 class="headerso">New Release: TITLE</h3>
//	  <span>M/D/YYYY</span>
var postRe = regexp.MustCompile(
	`location\.href='(news-articles/[^']+)'[^>]*>\s*` +
		`<img src="([^"]+)"[^>]*>[\s\S]*?` +
		`class="headerso">([^<]+)</h3>\s*<span>([^<]+)</span>`)

var archiveLinkRe = regexp.MustCompile(`href="(news-\d+\.html)"`)

// prefixFormats maps post-title prefixes to release formats. Order
// matters: more specific prefixes first ("New Release" is a prefix of
// none of these, but keep the pattern explicit).
var prefixFormats = []struct {
	prefix string
	format string
}{
	{"New Print Release:", model.FormatPhysical},
	{"New Audiobook Release:", model.FormatAudio},
	{"New Release:", model.FormatDigital},
}

func (c *ciw) Fetch(ctx context.Context, client *fetch.Client, mode source.Mode) ([]model.Release, error) {
	pages := []string{c.baseURL + "/"}
	seen := map[string]bool{}

	var out []model.Release
	for i := 0; i < len(pages) && i <= maxArchivePages; i++ {
		body, err := client.Get(ctx, pages[i])
		if err != nil {
			return nil, fmt.Errorf("crossinfinite: %w", err)
		}
		out = append(out, parsePosts(c.baseURL, string(body))...)

		// Full mode: queue newly discovered archive pages (news-1.html,
		// news-2.html, ... link to each other).
		if mode != source.ModeFull {
			break
		}
		for _, m := range archiveLinkRe.FindAllStringSubmatch(string(body), -1) {
			u := c.baseURL + "/" + m[1]
			if !seen[u] {
				seen[u] = true
				pages = append(pages, u)
			}
		}
	}
	return out, nil
}

func parsePosts(baseURL, body string) []model.Release {
	var out []model.Release
	for _, m := range postRe.FindAllStringSubmatch(body, -1) {
		articlePath, coverPath, header, dateStr := m[1], m[2], m[3], m[4]
		header = strings.TrimSpace(html.UnescapeString(header))

		format, title := "", ""
		for _, pf := range prefixFormats {
			if strings.HasPrefix(header, pf.prefix) {
				format = pf.format
				title = strings.TrimSpace(strings.TrimPrefix(header, pf.prefix))
				break
			}
		}
		if format == "" {
			continue // "New Volume:", license news, etc.
		}

		date, err := time.Parse("1/2/2006", strings.TrimSpace(dateStr))
		if err != nil {
			continue
		}

		out = append(out, model.Release{
			SeriesTitle: seriesFromTitle(title),
			VolumeTitle: title,
			Format:      format,
			ReleaseDate: model.DateOnly(date),
			URL:         baseURL + "/" + articlePath,
			CoverURL:    baseURL + "/" + coverPath,
		})
	}
	return out
}

// volSuffixRe strips trailing volume designations to recover the series
// title: "Vol. 3", "Vol.3", "Volume 4", including anything after them.
var volSuffixRe = regexp.MustCompile(`(?i)\s*Vol(?:ume)?\.?\s*\d+.*$`)

func seriesFromTitle(title string) string {
	s := strings.TrimSpace(volSuffixRe.ReplaceAllString(title, ""))
	if s == "" {
		return title
	}
	return s
}
