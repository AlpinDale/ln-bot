package bot

import (
	"strings"
	"testing"
)

func TestCleanURL(t *testing.T) {
	// Valid URLs pass through (unchanged or normalized).
	ok := map[string]string{
		"https://sevenseasentertainment.com/books/foo/": "https://sevenseasentertainment.com/books/foo/",
		"http://example.com":                            "http://example.com",
		"https://j-novel.club/series/x#volume-1":        "https://j-novel.club/series/x#volume-1",
		// Raw space and non-ASCII get percent-encoded so Discord accepts.
		"https://crossinfworld.com/news-articles/New Release Vol 1.html": "https://crossinfworld.com/news-articles/New%20Release%20Vol%201.html",
		"https://example.com/café":                                  "https://example.com/caf%C3%A9",
	}
	for in, want := range ok {
		got, valid := cleanURL(in)
		if !valid {
			t.Errorf("cleanURL(%q) = invalid, want %q", in, want)
			continue
		}
		if got != want {
			t.Errorf("cleanURL(%q) = %q, want %q", in, got, want)
		}
	}

	bad := []string{
		"",                  // empty
		"/books/relative",   // relative path (rendered-DOM hrefs)
		"books/foo.html",    // no scheme/host
		"ftp://example.com", // wrong scheme
		"javascript:alert(1)",
	}
	for _, u := range bad {
		if _, valid := cleanURL(u); valid {
			t.Errorf("cleanURL(%q) = valid, want invalid", u)
		}
	}
}

func TestTruncateRuneSafe(t *testing.T) {
	if got := truncate("short", 256); got != "short" {
		t.Errorf("unchanged: %q", got)
	}
	// 300 ASCII chars -> 256 runes, ellipsis included.
	long := strings.Repeat("a", 300)
	got := truncate(long, 256)
	if n := len([]rune(got)); n != 256 {
		t.Fatalf("want 256 runes, got %d", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("missing ellipsis: %q", got[len(got)-4:])
	}
	// Multibyte must not be split into invalid UTF-8.
	jp := strings.Repeat("日", 300)
	got = truncate(jp, 256)
	if n := len([]rune(got)); n != 256 {
		t.Fatalf("multibyte: want 256 runes, got %d", n)
	}
	for _, r := range got {
		if r == '�' {
			t.Fatal("truncate split a multibyte rune")
		}
	}
}
