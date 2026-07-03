package yenpress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestFetchMonthParsesNovelsOnly(t *testing.T) {
	fixture, err := os.ReadFile("testdata/calendar.html")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/calendar" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("imprint_id[]") != yenOnImprintID {
			t.Errorf("missing/wrong imprint filter: %q", q.Get("imprint_id[]"))
		}
		if q.Get("year") == "" || q.Get("month") == "" {
			t.Error("missing year/month params")
		}
		w.Write(fixture)
	}))
	defer srv.Close()

	src := &yen{baseURL: srv.URL}
	got, err := src.fetchMonth(context.Background(), testClient(), 2026, time.July)
	if err != nil {
		t.Fatal(err)
	}

	// Fixture: 2 novels + 1 manga (filtered by category tag).
	if len(got) != 2 {
		t.Fatalf("want 2 releases, got %d: %+v", len(got), got)
	}

	r := got[0]
	if r.VolumeTitle != "A Certain Magical Index NT, Vol. 6 (light novel)" {
		t.Errorf("title: %q", r.VolumeTitle)
	}
	if r.SeriesTitle != "A Certain Magical Index NT" {
		t.Errorf("series: %q", r.SeriesTitle)
	}
	if r.Format != model.FormatUnknown {
		t.Errorf("format: %q", r.Format)
	}
	if !r.ReleaseDate.Equal(time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date: %v", r.ReleaseDate)
	}
	if r.URL != srv.URL+"/titles/9781975388430-a-certain-magical-index-nt-vol-6-light-novel" {
		t.Errorf("url: %q", r.URL)
	}
	if r.CoverURL == "" {
		t.Error("cover missing")
	}
}

func TestFetchMonthIgnoresOtherMonthsItems(t *testing.T) {
	fixture, _ := os.ReadFile("testdata/calendar.html")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture)
	}))
	defer srv.Close()

	src := &yen{baseURL: srv.URL}
	// Fixture items say "Jul"; asking for August must yield nothing.
	got, err := src.fetchMonth(context.Background(), testClient(), 2026, time.August)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("Jul items leaked into Aug: %+v", got)
	}
}

func TestIncrementalWindowSize(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte("<html><body></body></html>"))
	}))
	defer srv.Close()

	src := &yen{baseURL: srv.URL}
	if _, err := src.Fetch(context.Background(), testClient(), source.ModeIncremental); err != nil {
		t.Fatal(err)
	}
	// prev month .. +4 months inclusive = 6 requests.
	if n := calls.Load(); n != 6 {
		t.Fatalf("want 6 month requests, got %d", n)
	}
}

func TestSeriesFromTitle(t *testing.T) {
	cases := map[string]string{
		"A Certain Magical Index NT, Vol. 6 (light novel)":    "A Certain Magical Index NT",
		"Sword Art Online 28 (light novel)":                   "Sword Art Online 28", // no Vol. marker: keep as-is
		"Re:ZERO Ex, Vol. 6 (novel)":                          "Re:ZERO Ex",
		"NieR:Automata: Long Story Short (light novel)":       "NieR:Automata: Long Story Short",
		"The Reincarnated Prince Becomes an Alchemist Vol. 3": "The Reincarnated Prince Becomes an Alchemist",
	}
	for in, want := range cases {
		if got := seriesFromTitle(in); got != want {
			t.Errorf("seriesFromTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
