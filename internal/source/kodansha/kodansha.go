// Package kodansha is the source plugin for Kodansha USA.
//
// Kodansha's WordPress site exposes a private JSON API (namespace
// kodansha/v1). Two endpoints combine into release data:
//
//   - /release-calendar: a fixed ~8-week window (≈2 weeks past to
//     ≈5 weeks forward) of releases grouped by Tuesday street date.
//     Items carry series name, volume title, formats, cover and URL —
//     but no series type.
//   - /search-series?series_types=novel: the catalog of prose series,
//     paginated by offset. Joining calendar items against this slug set
//     (via the /series/<slug>/ segment of volume_url) filters manga out.
//
// Limitations accepted by design: the window cannot be recentered, so
// full mode is identical to incremental (no backfill exists), and
// Kodansha's "novel" type includes some non-LN prose (language
// references etc.) — low-volume noise we tolerate.
package kodansha

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/source"
	"github.com/alpindale/ln-bot/internal/source/fetch"
)

const (
	defaultBaseURL = "https://kodansha.us"

	slugPageSize = 25   // fixed by the API (page/per_page are ignored)
	maxSlugPages = 40   // safety cap (~1000 novel series)
	slugCacheTTL = 20 * time.Hour
)

func init() {
	source.Register(&kod{baseURL: defaultBaseURL})
}

type kod struct {
	baseURL string // overridable in tests

	novelSlugs   map[string]bool
	slugsFetched time.Time
}

func (k *kod) Name() string      { return "kodansha" }
func (k *kod) Publisher() string { return "Kodansha USA" }

type calendarResponse struct {
	Success bool `json:"success"`
	Data    []struct {
		TueKey string `json:"tue_key"`
		Items  []struct {
			Title      string   `json:"title"`
			SeriesName string   `json:"series_name"`
			Image      string   `json:"image"`
			VolumeURL  string   `json:"volume_url"`
			Formats    []string `json:"formats"`
		} `json:"items"`
	} `json:"data"`
}

type seriesResponse struct {
	Success bool `json:"success"`
	Data    []struct {
		Slug string `json:"slug"`
		Type string `json:"type"`
	} `json:"data"`
	Count      int `json:"count"`
	TotalCount int `json:"total_count"`
}

var seriesSlugRe = regexp.MustCompile(`/series/([^/]+)/`)

// Fetch is mode-independent: the API serves one fixed window.
func (k *kod) Fetch(ctx context.Context, client *fetch.Client, _ source.Mode) ([]model.Release, error) {
	slugs, err := k.getNovelSlugs(ctx, client)
	if err != nil {
		return nil, err
	}

	body, err := client.Get(ctx, k.baseURL+"/wp-json/kodansha/v1/release-calendar")
	if err != nil {
		return nil, fmt.Errorf("kodansha: %w", err)
	}
	var cal calendarResponse
	if err := json.Unmarshal(body, &cal); err != nil {
		return nil, fmt.Errorf("kodansha: decode calendar: %w", err)
	}
	if !cal.Success {
		return nil, fmt.Errorf("kodansha: calendar returned success=false")
	}

	var out []model.Release
	for _, week := range cal.Data {
		date, err := time.Parse("2006-01-02", week.TueKey)
		if err != nil {
			continue
		}
		for _, item := range week.Items {
			m := seriesSlugRe.FindStringSubmatch(item.VolumeURL)
			if m == nil || !slugs[m[1]] {
				continue // manga or unknown series
			}
			title := item.Title
			if item.SeriesName != "" {
				title = item.SeriesName + " " + item.Title
			}
			for _, f := range item.Formats {
				out = append(out, model.Release{
					SeriesTitle: item.SeriesName,
					VolumeTitle: title,
					Format:      mapFormat(f),
					ReleaseDate: model.DateOnly(date),
					URL:         item.VolumeURL,
					CoverURL:    item.Image,
				})
			}
		}
	}
	return out, nil
}

// getNovelSlugs returns the cached set of novel-type series slugs,
// re-enumerating the catalog when the cache is stale.
func (k *kod) getNovelSlugs(ctx context.Context, client *fetch.Client) (map[string]bool, error) {
	if k.novelSlugs != nil && time.Since(k.slugsFetched) < slugCacheTTL {
		return k.novelSlugs, nil
	}

	slugs := map[string]bool{}
	for page := 0; page < maxSlugPages; page++ {
		url := fmt.Sprintf("%s/wp-json/kodansha/v1/search-series?series_types=novel&offset=%d",
			k.baseURL, page*slugPageSize)
		body, err := client.Get(ctx, url)
		if err != nil {
			return nil, fmt.Errorf("kodansha: novel catalog: %w", err)
		}
		var resp seriesResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("kodansha: decode catalog: %w", err)
		}
		for _, s := range resp.Data {
			if s.Type == "novel" {
				slugs[s.Slug] = true
			}
		}
		if len(resp.Data) == 0 || (resp.TotalCount > 0 && len(slugs) >= resp.TotalCount) {
			break
		}
	}
	if len(slugs) == 0 {
		return nil, fmt.Errorf("kodansha: novel catalog came back empty")
	}
	k.novelSlugs = slugs
	k.slugsFetched = time.Now()
	return slugs, nil
}

func mapFormat(f string) string {
	switch f {
	case "digital":
		return model.FormatDigital
	case "print":
		return model.FormatPhysical
	case "audio", "audiobook":
		return model.FormatAudio
	default:
		return model.FormatUnknown
	}
}
