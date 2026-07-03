package viz

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/source/fetch"
)

func testClient() *fetch.Client {
	return fetch.New(fetch.Options{MinDelay: time.Millisecond, Timeout: 5 * time.Second})
}

const calendarHTML = `<html><body>
<article class="g-3">
  <figure class="ar-square">
    <a href="/manga-books/novel/sakamoto-days-novels/product/8913/paperback" class="product-thumb">
      <img class="lazy" alt="" data-original="https://dw9to29mmj727.cloudfront.net/products/1974716325.jpg" />
    </a>
  </figure>
  <div>
    <div class="mar-b-sm"><a class="color-mid-gray hover-red">Novel</a></div>
    <a class="color-off-black hover-red" href="/manga-books/novel/sakamoto-days-novels/product/8913/paperback">Sakamoto Days: Assassin's Method</a>
  </div>
</article>
<article class="g-3">
  <figure class="ar-square">
    <a href="/manga-books/manga/one-piece/product/999/paperback" class="product-thumb">
      <img class="lazy" alt="" data-original="https://dw9to29mmj727.cloudfront.net/products/op.jpg" />
    </a>
  </figure>
  <div>
    <div class="mar-b-sm"><a class="color-mid-gray hover-red">Manga</a></div>
    <a class="color-off-black hover-red" href="/manga-books/manga/one-piece/product/999/paperback">One Piece, Vol. 109</a>
  </div>
</article>
</body></html>`

const productHTML = `<html><body>
<div class="g-6--md">
  <div class="mar-b-md"><strong>Original Concept by</strong> Yuto Suzuki</div>
  <div class="o_release-date mar-b-md"><strong>Release</strong> July 28, 2026</div>
  <div class="o_isbn13 mar-b-md"><strong>ISBN-13</strong> 978-1-9747-1632-6</div>
</div>
</body></html>`

func TestFetchMonthFiltersAndEnriches(t *testing.T) {
	var productFetches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/calendar/2026/07":
			fmt.Fprint(w, calendarHTML)
		case "/manga-books/novel/sakamoto-days-novels/product/8913/paperback":
			productFetches++
			fmt.Fprint(w, productHTML)
		default:
			t.Errorf("unexpected fetch: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	src := &viz{baseURL: srv.URL}
	got, err := src.fetchMonth(context.Background(), testClient(), 2026, 7)
	if err != nil {
		t.Fatal(err)
	}

	// Manga article filtered; only the novel remains, with the exact
	// date pulled from its product page (one enrichment fetch).
	if len(got) != 1 {
		t.Fatalf("want 1 release, got %d: %+v", len(got), got)
	}
	if productFetches != 1 {
		t.Fatalf("want 1 product fetch, got %d", productFetches)
	}

	r := got[0]
	if r.VolumeTitle != "Sakamoto Days: Assassin's Method" {
		t.Errorf("title: %q", r.VolumeTitle)
	}
	if r.SeriesTitle != "Sakamoto Days" {
		t.Errorf("series: %q", r.SeriesTitle)
	}
	if r.Format != model.FormatPhysical {
		t.Errorf("format: %q", r.Format)
	}
	if !r.ReleaseDate.Equal(time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date: %v", r.ReleaseDate)
	}
	if r.CoverURL != "https://dw9to29mmj727.cloudfront.net/products/1974716325.jpg" {
		t.Errorf("cover: %q", r.CoverURL)
	}
}

func TestSeriesFromSlug(t *testing.T) {
	cases := map[string]string{
		"/manga-books/novel/sakamoto-days-novels/product/8913/paperback": "Sakamoto Days",
		"/manga-books/novel/death-note-novel/product/1/paperback":        "Death Note",
		"/manga-books/novel/no-game-no-life/product/2/digital":           "No Game No Life",
	}
	for in, want := range cases {
		if got := seriesFromSlug(in); got != want {
			t.Errorf("seriesFromSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostDelayOverride(t *testing.T) {
	c := fetch.New(fetch.Options{
		MinDelay:   time.Millisecond,
		Timeout:    5 * time.Second,
		HostDelays: map[string]time.Duration{"viz.com": 50 * time.Millisecond},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	// Requests to the test server (127.0.0.1) use MinDelay, not the
	// viz.com override; this just asserts the client still works with
	// HostDelays set. The mapping itself is unit-logic in fetch.
	if _, err := c.Get(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
}
