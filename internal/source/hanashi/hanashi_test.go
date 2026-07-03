package hanashi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/source"
	"github.com/alpindale/ln-bot/internal/source/fetch"
)

func testClient() *fetch.Client {
	return fetch.New(fetch.Options{MinDelay: time.Millisecond, Timeout: 5 * time.Second})
}

func TestFetchParsesEbooksWithDates(t *testing.T) {
	fixture, err := os.ReadFile("testdata/search.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("term") != searchTerm || q.Get("entity") != "ebook" {
			t.Errorf("unexpected query: %v", q)
		}
		w.Write(fixture)
	}))
	defer srv.Close()

	src := &hanashi{baseURL: srv.URL}
	got, err := src.Fetch(context.Background(), testClient(), source.ModeIncremental)
	if err != nil {
		t.Fatal(err)
	}

	// Fixture: 4 ebooks + 1 audiobook (wrong kind) + 1 dateless ebook
	// → 4 kept.
	if len(got) != 4 {
		t.Fatalf("want 4 releases, got %d: %+v", len(got), got)
	}

	byTitle := map[string]model.Release{}
	for _, r := range got {
		byTitle[r.VolumeTitle] = r
	}

	tsu := byTitle["Tsukimichi - Volume 17 (Light Novel)"]
	if tsu.SeriesTitle != "Tsukimichi" {
		t.Errorf("series: %q", tsu.SeriesTitle)
	}
	if tsu.Format != model.FormatDigital {
		t.Errorf("format: %q", tsu.Format)
	}
	if !tsu.ReleaseDate.Equal(time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date: %v", tsu.ReleaseDate)
	}

	newgate := byTitle["The New Gate - Volume 4 (Light Novel)"]
	if newgate.SeriesTitle != "The New Gate" {
		t.Errorf("New Gate series: %q", newgate.SeriesTitle)
	}
	if !newgate.ReleaseDate.Equal(time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("preorder date: %v", newgate.ReleaseDate)
	}
	// Cover upgraded to 600px.
	if newgate.CoverURL != "" && !containsRes(newgate.CoverURL, "600x600bb") {
		t.Errorf("cover not upgraded: %q", newgate.CoverURL)
	}
}

func containsRes(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestSeriesFromTitle(t *testing.T) {
	cases := map[string]string{
		"Tsukimichi - Volume 17 (Light Novel)":                     "Tsukimichi",
		"GATE (Light Novel), Vol. 1 - Part II":                     "GATE",
		"The New Gate - Volume 4 (Light Novel)":                    "The New Gate",
		"My Pet is a Saintess - Volume 3":                          "My Pet is a Saintess",
		"An Observation Log of My Wife (Light Novel) - Vol. 1":     "An Observation Log of My Wife",
		"Tsukimichi - Volume 8.5 (Light Novel)":                    "Tsukimichi",
		"The Dark Guild Master's Smile Would Fit Best - Volume 1":  "The Dark Guild Master's Smile Would Fit Best",
	}
	for in, want := range cases {
		if got := seriesFromTitle(in); got != want {
			t.Errorf("seriesFromTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
