package bot

import (
	"strings"
	"testing"
)

func TestValidURL(t *testing.T) {
	good := []string{
		"https://sevenseasentertainment.com/books/foo/",
		"http://example.com",
		"https://j-novel.club/series/x#volume-1",
	}
	for _, u := range good {
		if !validURL(u) {
			t.Errorf("validURL(%q) = false, want true", u)
		}
	}
	bad := []string{
		"",                 // empty
		"/books/relative",  // relative path (rendered-DOM hrefs)
		"books/foo.html",   // no scheme/host
		"ftp://example.com", // wrong scheme
		"not a url",
		"javascript:alert(1)",
	}
	for _, u := range bad {
		if validURL(u) {
			t.Errorf("validURL(%q) = true, want false", u)
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
