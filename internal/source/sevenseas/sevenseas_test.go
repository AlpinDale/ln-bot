package sevenseas

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/source"
	"github.com/alpindale/ln-bot/internal/source/fetch"
)

const tableHTML = `<html><body><table>
<tr><th>Date</th><th>Title</th><th>Format</th><th>ISBN</th></tr>
<tr id="volumes">
  <td style="text-align: center;">2026-07-07</td>
  <td><a href="https://sevenseasentertainment.com/books/my-status-light-novel-vol-5/"><strong>My Status as an Assassin Obviously Exceeds the Hero's (Light Novel) Vol. 5</strong></a></td>
  <td>Light Novel</td>
  <td>978-1-63858-633-3</td>
</tr>
<tr id="volumes">
  <td>2026-07-07</td>
  <td><a href="https://sevenseasentertainment.com/books/i-got-caught-up-manga-vol-10/"><strong>I Got Caught Up In a Hero Summons (Manga) Vol. 10</strong></a></td>
  <td>Manga</td>
  <td>979-8-89561-681-9</td>
</tr>
<tr id="volumes">
  <td>2026-07-09</td>
  <td><a href="https://sevenseasentertainment.com/audio_books/trapped-in-a-dating-sim-light-novel-audiobook-vol-10/"><strong>Trapped in a Dating Sim (Light Novel) (Audiobook) Vol. 10</strong></a></td>
  <td>Audiobook</td>
  <td></td>
</tr>
<tr id="volumes">
  <td>2026-07-14</td>
  <td><a href="https://sevenseasentertainment.com/audio_books/some-manga-audiobook-vol-2/"><strong>Some Manga Audiobook Vol. 2</strong></a></td>
  <td>Audiobook</td>
  <td></td>
</tr>
<tr id="volumes">
  <td>2026-07-21</td>
  <td><a href="https://sevenseasentertainment.com/books/case-file-compendium-novel-vol-10/"><strong>Case File Compendium (Novel) Vol. 10</strong></a></td>
  <td>Novel</td>
  <td>979-8-88843-000-0</td>
</tr>
</table></body></html>`

func testClient() *fetch.Client {
	return fetch.New(fetch.Options{MinDelay: time.Millisecond, Timeout: 5 * time.Second})
}

func TestFetchParsesLightNovelsOnly(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/release-dates/archive/" {
			w.Write([]byte("<html><body><table></table></body></html>"))
			return
		}
		w.Write([]byte(tableHTML))
	}))
	defer srv.Close()

	src := &ss{baseURL: srv.URL}
	got, err := src.Fetch(context.Background(), testClient(), source.ModeIncremental)
	if err != nil {
		t.Fatal(err)
	}

	// Both list pages fetched.
	if len(paths) != 2 {
		t.Fatalf("want 2 page fetches, got %v", paths)
	}

	// From 5 rows: LN, manga(skip), LN-audiobook, manga-audiobook(skip),
	// Novel → 3 kept.
	if len(got) != 3 {
		t.Fatalf("want 3 releases, got %d: %+v", len(got), got)
	}

	if got[0].VolumeTitle != "My Status as an Assassin Obviously Exceeds the Hero's (Light Novel) Vol. 5" {
		t.Errorf("title: %q", got[0].VolumeTitle)
	}
	if got[0].SeriesTitle != "My Status as an Assassin Obviously Exceeds the Hero's" {
		t.Errorf("series: %q", got[0].SeriesTitle)
	}
	if got[0].Format != model.FormatUnknown {
		t.Errorf("LN format: %q", got[0].Format)
	}
	if !got[0].ReleaseDate.Equal(time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date: %v", got[0].ReleaseDate)
	}
	if got[1].Format != model.FormatAudio {
		t.Errorf("LN audiobook format: %q", got[1].Format)
	}
	if got[2].SeriesTitle != "Case File Compendium" {
		t.Errorf("novel series: %q", got[2].SeriesTitle)
	}
}

func TestSeriesFromTitle(t *testing.T) {
	cases := map[string]string{
		"My Status (Light Novel) Vol. 5":       "My Status",
		"Case File Compendium (Novel) Vol. 10": "Case File Compendium",
		"Mushoku Tensei (Light Novel) Omnibus 3": "Mushoku Tensei (Light Novel) Omnibus 3",
	}
	for in, want := range cases {
		if got := seriesFromTitle(in); got != want {
			t.Errorf("seriesFromTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
