package kodansha

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/source"
	"github.com/alpindale/ln-bot/internal/source/fetch"
)

func testClient() *fetch.Client {
	return fetch.New(fetch.Options{MinDelay: time.Millisecond, Timeout: 5 * time.Second})
}

const calendarJSON = `{"success":true,"data":[
 {"tue_key":"2026-07-07","date_label":"...","is_past":false,"items":[
   {"title":"Volume 12","series_name":"Quality Assurance in Another World",
    "creators":"By Masamichi Sato","image":"https://production.image.azuki.co/qa12/800.webp",
    "volume_url":"https://kodansha.us/series/quality-assurance-in-another-world/volume-12/",
    "formats":["digital","print"]},
   {"title":"Volume 38","series_name":"Blue Lock",
    "creators":"By Muneyuki Kaneshiro","image":"https://production.image.azuki.co/bl38/800.webp",
    "volume_url":"https://kodansha.us/series/blue-lock/volume-38/",
    "formats":["digital"]}
 ]},
 {"tue_key":"2026-07-14","date_label":"...","is_past":false,"items":[
   {"title":"Volume 3","series_name":"Faraway Paladin",
    "creators":"By Kanata Yanagino","image":"https://production.image.azuki.co/fp3/800.webp",
    "volume_url":"https://kodansha.us/series/faraway-paladin-novel/volume-3/",
    "formats":["digital"]}
 ]}
]}`

// Two catalog pages: page 1 full-page-size would be 25, but our fixture
// returns 2 then 0 to end pagination via empty page.
func seriesHandler(t *testing.T, calls *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.URL.Query().Get("series_types"); got != "novel" {
			t.Errorf("series_types = %q, want novel (plural!)", got)
		}
		switch r.URL.Query().Get("offset") {
		case "0":
			fmt.Fprint(w, `{"success":true,"data":[
				{"slug":"quality-assurance-in-another-world","type":"novel"},
				{"slug":"faraway-paladin-novel","type":"novel"}],
				"count":2,"total_count":2}`)
		default:
			fmt.Fprint(w, `{"success":true,"data":[],"count":0,"total_count":2}`)
		}
	}
}

func newTestServer(t *testing.T, catalogCalls *atomic.Int32) *httptest.Server {
	sh := seriesHandler(t, catalogCalls)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/wp-json/kodansha/v1/release-calendar":
			fmt.Fprint(w, calendarJSON)
		case "/wp-json/kodansha/v1/search-series":
			sh(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestFetchJoinsNovelSlugs(t *testing.T) {
	var catalogCalls atomic.Int32
	srv := newTestServer(t, &catalogCalls)
	defer srv.Close()

	src := &kod{baseURL: srv.URL}
	got, err := src.Fetch(context.Background(), testClient(), source.ModeIncremental)
	if err != nil {
		t.Fatal(err)
	}

	// QA in Another World (digital+print = 2 rows) + Faraway Paladin
	// (1 row); Blue Lock is manga (not in novel slug set) → dropped.
	if len(got) != 3 {
		t.Fatalf("want 3 releases, got %d: %+v", len(got), got)
	}

	r := got[0]
	if r.VolumeTitle != "Quality Assurance in Another World Volume 12" {
		t.Errorf("title: %q", r.VolumeTitle)
	}
	if r.SeriesTitle != "Quality Assurance in Another World" {
		t.Errorf("series: %q", r.SeriesTitle)
	}
	if r.Format != model.FormatDigital || got[1].Format != model.FormatPhysical {
		t.Errorf("formats: %q, %q", r.Format, got[1].Format)
	}
	if !r.ReleaseDate.Equal(time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date: %v", r.ReleaseDate)
	}
	if !got[2].ReleaseDate.Equal(time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("week 2 date: %v", got[2].ReleaseDate)
	}
	if r.CoverURL == "" || r.URL == "" {
		t.Error("missing cover/url")
	}
}

func TestSlugCacheAvoidsRefetch(t *testing.T) {
	var catalogCalls atomic.Int32
	srv := newTestServer(t, &catalogCalls)
	defer srv.Close()

	src := &kod{baseURL: srv.URL}
	ctx := context.Background()
	if _, err := src.Fetch(ctx, testClient(), source.ModeIncremental); err != nil {
		t.Fatal(err)
	}
	first := catalogCalls.Load()
	if first == 0 {
		t.Fatal("catalog never fetched")
	}
	if _, err := src.Fetch(ctx, testClient(), source.ModeFull); err != nil {
		t.Fatal(err)
	}
	if catalogCalls.Load() != first {
		t.Fatalf("catalog refetched despite warm cache: %d -> %d", first, catalogCalls.Load())
	}
}
