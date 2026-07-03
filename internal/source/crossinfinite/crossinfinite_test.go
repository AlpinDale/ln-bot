package crossinfinite

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

func fixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	home, err := os.ReadFile("testdata/home.html")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile("testdata/news-1.html")
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Write(home)
		case "/news-1.html":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestIncrementalParsesHomepageOnly(t *testing.T) {
	srv := fixtureServer(t)
	defer srv.Close()

	src := &ciw{baseURL: srv.URL}
	got, err := src.Fetch(context.Background(), testClient(), source.ModeIncremental)
	if err != nil {
		t.Fatal(err)
	}

	// Home fixture: 5 posts — digital, announcement (skip), print,
	// audiobook, license (skip) → 3 releases.
	if len(got) != 3 {
		t.Fatalf("want 3 releases, got %d: %+v", len(got), got)
	}

	r := got[0]
	if r.VolumeTitle != "Love & Magic Academy Vol. 3" {
		t.Errorf("title: %q", r.VolumeTitle)
	}
	if r.SeriesTitle != "Love & Magic Academy" {
		t.Errorf("series: %q", r.SeriesTitle)
	}
	if r.Format != model.FormatDigital {
		t.Errorf("format: %q", r.Format)
	}
	if !r.ReleaseDate.Equal(time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date: %v", r.ReleaseDate)
	}
	if r.URL != srv.URL+"/news-articles/New-Release-Love-and-Magic-Academy-Vol-3.html" {
		t.Errorf("url: %q", r.URL)
	}
	if r.CoverURL != srv.URL+"/img/lncover/LMA-vol-3-cover-web.jpg" {
		t.Errorf("cover: %q", r.CoverURL)
	}

	if got[1].Format != model.FormatPhysical {
		t.Errorf("print post format: %q", got[1].Format)
	}
	if got[2].Format != model.FormatAudio {
		t.Errorf("audiobook post format: %q", got[2].Format)
	}
	// "Volume N" suffix form also stripped for series.
	if got[1].SeriesTitle != "Even Dogs Go to Other Worlds" {
		t.Errorf("series from 'Vol.N': %q", got[1].SeriesTitle)
	}
}

func TestFullModeWalksArchive(t *testing.T) {
	srv := fixtureServer(t)
	defer srv.Close()

	src := &ciw{baseURL: srv.URL}
	got, err := src.Fetch(context.Background(), testClient(), source.ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	// 3 from home + 1 from news-1.html; the archive's self-link must
	// not cause a refetch loop.
	if len(got) != 4 {
		t.Fatalf("want 4 releases in full mode, got %d", len(got))
	}
	last := got[3]
	if last.VolumeTitle != "Fluffy Paradise Volume 8" || last.SeriesTitle != "Fluffy Paradise" {
		t.Errorf("archive release wrong: %+v", last)
	}
}
