// Package hanashi is the source plugin for Hanashi Media.
//
// Hanashi's own sites are unusable for release tracking: both are behind
// a Cloudflare managed challenge, and neither publishes machine-readable
// street dates (the WooCommerce store keeps preorder dates in private
// plugin meta; the marketing site is a dateless Elementor build).
//
// Instead we source Hanashi's catalog from Apple Books via the public
// iTunes Search API, which lists every Hanashi ebook — including
// preorders — with a real releaseDate, cover art and product URL, and
// needs no auth or challenge solving. The API does not expose the seller
// field, so the publisher filter is the search term itself: "Hanashi
// Media" is distinctive enough that matches are reliably Hanashi's.
package hanashi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/source"
	"github.com/alpindale/ln-bot/internal/source/fetch"
)

const (
	defaultBaseURL = "https://itunes.apple.com"
	searchTerm     = "hanashi media"
	// iTunes Search caps results and offers no pagination; 200 is the
	// max and comfortably exceeds Hanashi's catalog (~80 and growing).
	searchLimit = 200
)

func init() {
	source.Register(&hanashi{baseURL: defaultBaseURL})
}

type hanashi struct {
	baseURL string // overridable in tests
}

func (h *hanashi) Name() string      { return "hanashi" }
func (h *hanashi) Publisher() string { return "Hanashi Media" }

type searchResponse struct {
	ResultCount int `json:"resultCount"`
	Results     []struct {
		TrackName     string `json:"trackName"`
		ArtistName    string `json:"artistName"`
		Kind          string `json:"kind"`
		ReleaseDate   string `json:"releaseDate"`
		TrackViewURL  string `json:"trackViewUrl"`
		ArtworkURL100 string `json:"artworkUrl100"`
	} `json:"results"`
}

// Fetch is mode-independent: one search returns the whole catalog
// (past releases and future preorders alike).
func (h *hanashi) Fetch(ctx context.Context, client *fetch.Client, _ source.Mode) ([]model.Release, error) {
	q := url.Values{
		"term":    {searchTerm},
		"entity":  {"ebook"},
		"country": {"us"},
		"limit":   {fmt.Sprint(searchLimit)},
	}
	body, err := client.Get(ctx, h.baseURL+"/search?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("hanashi: %w", err)
	}
	var resp searchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("hanashi: decode search: %w", err)
	}

	var out []model.Release
	for _, r := range resp.Results {
		if r.Kind != "ebook" || r.TrackName == "" || r.ReleaseDate == "" {
			continue
		}
		date, err := time.Parse(time.RFC3339, r.ReleaseDate)
		if err != nil {
			continue
		}
		out = append(out, model.Release{
			SeriesTitle: seriesFromTitle(r.TrackName),
			VolumeTitle: r.TrackName,
			Format:      model.FormatDigital,
			ReleaseDate: model.DateOnly(date),
			URL:         r.TrackViewURL,
			CoverURL:    upgradeArtwork(r.ArtworkURL100),
		})
	}
	return out, nil
}

// upgradeArtwork swaps the iTunes 100px thumbnail for a 600px cover.
func upgradeArtwork(u string) string {
	if u == "" {
		return ""
	}
	return strings.Replace(u, "100x100bb", "600x600bb", 1)
}

// volSuffixRe strips volume markers used in Apple titles, e.g.
// " - Volume 17 (Light Novel)", " (Light Novel), Vol. 13",
// " (Light Novel) - Vol. 1".
var volSuffixRe = regexp.MustCompile(`(?i)\s*[-,]?\s*(?:\(light novel\)\s*)?[-,]?\s*Vol(?:ume)?\.?\s*[\d.]+.*$`)
var lnParenRe = regexp.MustCompile(`(?i)\s*\(light novel\)\s*$`)

func seriesFromTitle(title string) string {
	s := volSuffixRe.ReplaceAllString(title, "")
	s = lnParenRe.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	if s == "" {
		return title
	}
	return s
}
