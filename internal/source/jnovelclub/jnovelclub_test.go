package jnovelclub

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/source/fetch"
)

func testClient() *fetch.Client {
	return fetch.New(fetch.Options{MinDelay: time.Millisecond, Timeout: 5 * time.Second})
}

func TestFetchFiltersToNovelEbookReleases(t *testing.T) {
	fixture, err := os.ReadFile("testdata/events.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/v2/events" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		for _, p := range []string{"start_date", "end_date", "limit", "skip"} {
			if r.URL.Query().Get(p) == "" {
				t.Errorf("missing query param %s", p)
			}
		}
		w.Write(fixture)
	}))
	defer srv.Close()

	src := &jnc{baseURL: srv.URL}
	got, err := src.Fetch(context.Background(), testClient())
	if err != nil {
		t.Fatal(err)
	}

	// Fixture holds 5 events: 2 valid novel ebooks, 1 manga ebook,
	// 1 prepub part, 1 unparseable date.
	if len(got) != 2 {
		t.Fatalf("want 2 releases, got %d: %+v", len(got), got)
	}

	r := got[0]
	if r.VolumeTitle != "Otherside Picnic: Volume 10" {
		t.Errorf("title: %q", r.VolumeTitle)
	}
	if r.SeriesTitle != "Otherside Picnic" {
		t.Errorf("series: %q", r.SeriesTitle)
	}
	if r.Format != model.FormatDigital {
		t.Errorf("format: %q", r.Format)
	}
	if !r.ReleaseDate.Equal(time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date: %v", r.ReleaseDate)
	}
	if r.URL != "https://j-novel.club/series/otherside-picnic" {
		t.Errorf("url: %q", r.URL)
	}
	// Event-specific thumbnail preferred over series cover.
	if r.CoverURL != "https://cdn.example/otherside-vol10.jpg" {
		t.Errorf("cover: %q", r.CoverURL)
	}

	// Second release has no event thumbnail — series cover fallback.
	if got[1].CoverURL != "https://cdn.example/tanaka-series-cover.jpg" {
		t.Errorf("cover fallback: %q", got[1].CoverURL)
	}
}

func TestFetchPaginates(t *testing.T) {
	page := func(names []string, last bool) string {
		evs := ""
		for i, n := range names {
			if i > 0 {
				evs += ","
			}
			evs += fmt.Sprintf(`{
				"name": %q, "details": "Ebook Publishing",
				"launch": "2026-08-01T14:00:00Z",
				"serie": {"type": "NOVEL", "title": %q, "slug": "s",
					"cover": {"coverUrl": "", "thumbnailUrl": ""}},
				"thumbnail": {"coverUrl": "", "thumbnailUrl": ""}
			}`, n, n)
		}
		return fmt.Sprintf(`{"events":[%s],"pagination":{"lastPage":%v}}`, evs, last)
	}

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Query().Get("skip") {
		case "0":
			fmt.Fprint(w, page([]string{"Vol A"}, false))
		case "200":
			fmt.Fprint(w, page([]string{"Vol B"}, true))
		default:
			t.Errorf("unexpected skip %q", r.URL.Query().Get("skip"))
			fmt.Fprint(w, page(nil, true))
		}
	}))
	defer srv.Close()

	src := &jnc{baseURL: srv.URL}
	got, err := src.Fetch(context.Background(), testClient())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("want 2 API calls, got %d", calls)
	}
	if len(got) != 2 || got[0].VolumeTitle != "Vol A" || got[1].VolumeTitle != "Vol B" {
		t.Fatalf("pagination results wrong: %+v", got)
	}
}
