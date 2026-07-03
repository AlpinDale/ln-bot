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

// bigHTML is a >2KB HTML document (passes looksLikeHTMLDoc); the marker
// lets tests assert the right body was returned.
func bigHTML(marker string) string {
	return "<!doctype html><html><body>" + marker + strings.Repeat("x", 3000) + "</body></html>"
}

// resultJSON builds a scan result with the given text/html response
// hashes under data.requests and the same under lists.hashes.
func resultJSON(htmlHashes ...string) string {
	var reqs []string
	for _, h := range htmlHashes {
		reqs = append(reqs, fmt.Sprintf(`{"response":{"hash":%q,"response":{"mimeType":"text/html"}}}`, h))
	}
	var lists []string
	for _, h := range htmlHashes {
		lists = append(lists, fmt.Sprintf("%q", h))
	}
	return fmt.Sprintf(`{"task":{"success":true},"data":{"requests":[%s]},"lists":{"hashes":[%s]}}`,
		strings.Join(reqs, ","), strings.Join(lists, ","))
}

// A CF-scanner host routes through the URL Scanner API: create scan →
// poll result → return the largest captured text/html document.
func TestCFScannerRouting(t *testing.T) {
	stub := bigHTML("STUB")   // small-ish decoy
	page := bigHTML("REALPAGE") + strings.Repeat("y", 5000) // larger = the real page
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
			fmt.Fprint(w, resultJSON("hstub", "hpage"))
		case strings.HasSuffix(r.URL.Path, "/responses/hstub"):
			fmt.Fprint(w, stub)
		case strings.HasSuffix(r.URL.Path, "/responses/hpage"):
			fmt.Fprint(w, page)
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
	// Largest HTML wins — the real page, not the smaller stub.
	if !strings.Contains(string(body), "REALPAGE") {
		t.Fatalf("did not pick the largest HTML doc (%d bytes)", len(body))
	}
}

// The result endpoint 404s until the scan finishes; keep polling.
func TestCFScannerPollsUntilReady(t *testing.T) {
	page := bigHTML("READY")
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
			fmt.Fprint(w, resultJSON("h"))
		case strings.HasSuffix(r.URL.Path, "/responses/h"):
			fmt.Fprint(w, page)
		}
	}))
	defer srv.Close()

	c := newCFTestClient(srv.URL)
	body, err := c.Get(context.Background(), "https://sevenseasentertainment.com/x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "READY") || polls != 3 {
		t.Fatalf("body-has-READY=%v polls=%d", strings.Contains(string(body), "READY"), polls)
	}
}

// A 409 on create falls back to searching for the most recent scan.
func TestCFScannerConflictUsesSearch(t *testing.T) {
	page := bigHTML("FROMSEARCH")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/scan"):
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"errors":[{"code":409}]}`)
		case strings.HasSuffix(r.URL.Path, "/search"):
			fmt.Fprint(w, `{"results":[{"task":{"uuid":"recent"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/result/recent"):
			fmt.Fprint(w, resultJSON("h"))
		case strings.HasSuffix(r.URL.Path, "/responses/h"):
			fmt.Fprint(w, page)
		}
	}))
	defer srv.Close()

	c := newCFTestClient(srv.URL)
	body, err := c.Get(context.Background(), "https://sevenseasentertainment.com/y")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "FROMSEARCH") {
		t.Fatalf("wrong body: %q", body[:min(40, len(body))])
	}
}

// When data.requests has no HTML entries (schema drift), fall back to
// scanning the full hash list for the main document.
func TestCFScannerFallbackScansHashes(t *testing.T) {
	page := bigHTML("FALLBACK")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/scan"):
			fmt.Fprint(w, `{"uuid":"u"}`)
		case strings.HasSuffix(r.URL.Path, "/result/u"):
			// No data.requests; only a bare hash list to scan.
			fmt.Fprint(w, `{"task":{"success":true},"lists":{"hashes":["tiny","doc"]}}`)
		case strings.HasSuffix(r.URL.Path, "/responses/tiny"):
			fmt.Fprint(w, "<html>too small</html>") // < 2KB, skipped
		case strings.HasSuffix(r.URL.Path, "/responses/doc"):
			fmt.Fprint(w, page)
		}
	}))
	defer srv.Close()

	c := newCFTestClient(srv.URL)
	body, err := c.Get(context.Background(), "https://sevenseasentertainment.com/z")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "FALLBACK") {
		t.Fatalf("fallback picked wrong body (%d bytes)", len(body))
	}
}

// A scan that captures the anti-bot challenge page is retried with a
// fresh scan until it gets the real document.
func TestCFScannerRetriesOnChallenge(t *testing.T) {
	page := bigHTML("REALPAGE")
	challenge := "<html><head><title>Robot Challenge Screen</title></head>" +
		"<body>" + strings.Repeat("z", 3000) + " sgcaptcha</body></html>"
	var scans int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/scan"):
			scans++
			fmt.Fprintf(w, `{"uuid":"u%d"}`, scans)
		case strings.Contains(r.URL.Path, "/result/"):
			fmt.Fprint(w, resultJSON("h"))
		case strings.HasSuffix(r.URL.Path, "/responses/h"):
			if scans < 2 {
				fmt.Fprint(w, challenge) // first scan: challenge screen
			} else {
				fmt.Fprint(w, page) // second scan: real page
			}
		}
	}))
	defer srv.Close()

	c := newCFTestClient(srv.URL)
	body, err := c.Get(context.Background(), "https://sevenseasentertainment.com/release-dates/")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "REALPAGE") {
		t.Fatalf("expected real page after retry, got %d bytes", len(body))
	}
	if scans < 2 {
		t.Fatalf("expected a fresh rescan, only %d scan(s)", scans)
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
	c.cfRescanWait = time.Millisecond
	return c
}
