package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A CF-scanner host routes through the URL Scanner API: create scan →
// poll result → fetch the rendered DOM of the main page.
func TestCFScannerRouting(t *testing.T) {
	const wantBody = "<html>SEVEN SEAS RELEASE TABLE</html>"
	var created bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/scan"):
			if r.Header.Get("Authorization") != "Bearer tok" {
				t.Errorf("missing bearer token")
			}
			created = true
			fmt.Fprint(w, `{"uuid":"abc-123"}`)
		case strings.HasSuffix(r.URL.Path, "/result/abc-123"):
			if !created {
				t.Error("polled result before creating scan")
			}
			fmt.Fprint(w, `{"task":{"success":true}}`)
		case strings.HasSuffix(r.URL.Path, "/dom/abc-123"):
			fmt.Fprint(w, wantBody)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newCFTestClient(srv.URL)
	body, err := c.Get(context.Background(), "https://sevenseasentertainment.com/release-dates/")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != wantBody {
		t.Fatalf("unexpected body: %q", body)
	}
}

// The result endpoint 404s until the scan finishes; the client must keep
// polling rather than error out.
func TestCFScannerPollsUntilReady(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/scan"):
			fmt.Fprint(w, `{"uuid":"u"}`)
		case strings.HasSuffix(r.URL.Path, "/result/u"):
			polls++
			if polls < 3 {
				http.NotFound(w, r) // still processing
				return
			}
			fmt.Fprint(w, `{"task":{"success":true}}`)
		case strings.HasSuffix(r.URL.Path, "/dom/u"):
			fmt.Fprint(w, "ready")
		}
	}))
	defer srv.Close()

	c := newCFTestClient(srv.URL)
	body, err := c.Get(context.Background(), "https://sevenseasentertainment.com/x")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ready" || polls != 3 {
		t.Fatalf("body=%q polls=%d", body, polls)
	}
}

// A 409 on create falls back to searching for the most recent scan.
func TestCFScannerConflictUsesSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/scan"):
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"errors":[{"code":409}]}`)
		case strings.HasSuffix(r.URL.Path, "/search"):
			fmt.Fprint(w, `{"tasks":[{"uuid":"recent"}]}`)
		case strings.HasSuffix(r.URL.Path, "/result/recent"):
			fmt.Fprint(w, `{"task":{"success":true}}`)
		case strings.HasSuffix(r.URL.Path, "/dom/recent"):
			fmt.Fprint(w, "from-search")
		}
	}))
	defer srv.Close()

	c := newCFTestClient(srv.URL)
	body, err := c.Get(context.Background(), "https://sevenseasentertainment.com/y")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "from-search" {
		t.Fatalf("body=%q", body)
	}
}

// Non-CF hosts fetch directly even when CF Scanner is configured.
func TestCFScannerOnlyForListedHosts(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("direct"))
	}))
	defer origin.Close()

	c := newCFTestClient("http://cf.invalid")
	body, err := c.Get(context.Background(), origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "direct" {
		t.Fatalf("expected direct fetch, got %q", body)
	}
}

// newCFTestClient builds a client whose CF API base points at srv and
// which routes sevenseasentertainment.com through it. It reaches into the
// unexported cfAPI field to redirect the base URL at the test server.
func newCFTestClient(apiBase string) *Client {
	c := New(Options{
		MinDelay:         time.Millisecond,
		Timeout:          5 * time.Second,
		MaxRetries:       1,
		CFScannerAccount: "acct",
		CFScannerToken:   "tok",
		CFScannerHosts:   []string{"sevenseasentertainment.com"},
	})
	c.cfAPI = apiBase
	c.cfPollWait = time.Millisecond // fast polling for tests
	return c
}

