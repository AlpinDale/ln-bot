package fetch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A FlareSolverr host routes through the proxy; the proxy's solved
// response body is what Get returns.
func TestFlareSolverrRouting(t *testing.T) {
	var gotCmd, gotURL string
	flare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1" || r.Method != http.MethodPost {
			t.Errorf("unexpected flare call: %s %s", r.Method, r.URL.Path)
		}
		var body struct{ Cmd, URL string }
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		gotCmd, gotURL = body.Cmd, body.URL
		w.Write([]byte(`{"status":"ok","message":"Challenge solved!",
			"solution":{"status":200,"response":"<html>SOLVED BODY</html>"}}`))
	}))
	defer flare.Close()

	c := New(Options{
		MinDelay:          time.Millisecond,
		Timeout:           5 * time.Second,
		FlareSolverrURL:   flare.URL,
		FlareSolverrHosts: []string{"sevenseasentertainment.com"},
	})

	body, err := c.Get(context.Background(), "https://sevenseasentertainment.com/release-dates/")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "<html>SOLVED BODY</html>" {
		t.Fatalf("unexpected body: %q", body)
	}
	if gotCmd != "request.get" {
		t.Errorf("cmd: %q", gotCmd)
	}
	if gotURL != "https://sevenseasentertainment.com/release-dates/" {
		t.Errorf("target url: %q", gotURL)
	}
}

// A non-FlareSolverr host is fetched directly, even when a FlareSolverr
// URL is configured.
func TestFlareSolverrOnlyForListedHosts(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("direct"))
	}))
	defer origin.Close()

	c := New(Options{
		MinDelay:          time.Millisecond,
		Timeout:           5 * time.Second,
		FlareSolverrURL:   "http://flaresolverr.invalid:8191",
		FlareSolverrHosts: []string{"sevenseasentertainment.com"},
	})

	body, err := c.Get(context.Background(), origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "direct" {
		t.Fatalf("expected direct fetch, got %q", body)
	}
}

// When FlareSolverr reports failure, Get surfaces a retryable error.
func TestFlareSolverrFailureRetryable(t *testing.T) {
	flare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"error","message":"challenge not solved"}`))
	}))
	defer flare.Close()

	c := New(Options{
		MinDelay:          time.Millisecond,
		Timeout:           5 * time.Second,
		MaxRetries:        1,
		FlareSolverrURL:   flare.URL,
		FlareSolverrHosts: []string{"sevenseasentertainment.com"},
	})

	_, err := c.Get(context.Background(), "https://sevenseasentertainment.com/x")
	if err == nil {
		t.Fatal("expected error from failed challenge")
	}
}
