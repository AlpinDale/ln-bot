// Package jnovelclub is the source plugin for J-Novel Club.
//
// It uses the public labs.j-novel.club app API's events feed, which
// lists "Ebook Publishing" events (full volume releases) alongside
// "Prepub Publishing" ones (weekly parts). Only full novel volumes are
// reported: prepub parts and manga are filtered out.
package jnovelclub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/source"
	"github.com/alpindale/ln-bot/internal/source/fetch"
)

const (
	defaultBaseURL = "https://labs.j-novel.club"
	siteURL        = "https://j-novel.club"

	// How far around "now" to ask the events API for.
	windowBack    = 7 * 24 * time.Hour
	windowForward = 90 * 24 * time.Hour

	pageLimit = 200
	// Safety cap on pagination; the window above fits comfortably.
	maxPages = 10
)

func init() {
	source.Register(&jnc{baseURL: defaultBaseURL})
}

type jnc struct {
	baseURL string // overridable in tests
}

func (j *jnc) Name() string      { return "jnovelclub" }
func (j *jnc) Publisher() string { return "J-Novel Club" }

// API response shapes (only the fields we consume).
type eventsResponse struct {
	Events []struct {
		Name    string `json:"name"`
		Details string `json:"details"`
		Launch  string `json:"launch"`
		Serie   struct {
			Type  string `json:"type"`
			Title string `json:"title"`
			Slug  string `json:"slug"`
			Cover struct {
				CoverURL     string `json:"coverUrl"`
				ThumbnailURL string `json:"thumbnailUrl"`
			} `json:"cover"`
		} `json:"serie"`
		Thumbnail struct {
			CoverURL     string `json:"coverUrl"`
			ThumbnailURL string `json:"thumbnailUrl"`
		} `json:"thumbnail"`
	} `json:"events"`
	Pagination struct {
		LastPage bool `json:"lastPage"`
	} `json:"pagination"`
}

func (j *jnc) Fetch(ctx context.Context, c *fetch.Client) ([]model.Release, error) {
	now := time.Now().UTC()
	start := now.Add(-windowBack).Format(time.RFC3339)
	end := now.Add(windowForward).Format(time.RFC3339)

	var out []model.Release
	for page := 0; page < maxPages; page++ {
		q := url.Values{
			"start_date": {start},
			"end_date":   {end},
			"limit":      {fmt.Sprint(pageLimit)},
			"skip":       {fmt.Sprint(page * pageLimit)},
			"format":     {"json"},
		}
		body, err := c.Get(ctx, j.baseURL+"/app/v2/events?"+q.Encode())
		if err != nil {
			return nil, fmt.Errorf("jnovelclub: %w", err)
		}
		var resp eventsResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("jnovelclub: decode events: %w", err)
		}

		for _, ev := range resp.Events {
			// Full novel volume releases only: no weekly prepub parts,
			// no manga.
			if ev.Details != "Ebook Publishing" || ev.Serie.Type != "NOVEL" {
				continue
			}
			launch, err := time.Parse(time.RFC3339, ev.Launch)
			if err != nil {
				continue // placeholder/garbage dates (e.g. hiatus markers)
			}
			cover := ev.Thumbnail.CoverURL
			if cover == "" {
				cover = ev.Serie.Cover.CoverURL
			}
			rel := model.Release{
				Publisher:   j.Publisher(),
				SeriesTitle: ev.Serie.Title,
				VolumeTitle: ev.Name,
				Format:      model.FormatDigital,
				ReleaseDate: model.DateOnly(launch),
				CoverURL:    cover,
			}
			if ev.Serie.Slug != "" {
				rel.URL = siteURL + "/series/" + ev.Serie.Slug
			}
			out = append(out, rel)
		}

		if resp.Pagination.LastPage || len(resp.Events) == 0 {
			break
		}
	}
	return out, nil
}
