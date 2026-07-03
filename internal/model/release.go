// Package model defines the shared domain types passed between source
// plugins, the store, and the Discord layer.
package model

import (
	"regexp"
	"strings"
	"time"
)

// Release formats.
const (
	FormatDigital  = "digital"
	FormatPhysical = "physical"
	FormatAudio    = "audio"
	FormatUnknown  = "unknown"
)

// Release is a single officially licensed English LN release as reported
// by a source plugin. ReleaseDate carries date-only semantics in the
// publisher's local terms; time-of-day is ignored.
type Release struct {
	ID          int64
	SourceKey   string
	Publisher   string
	SeriesTitle string
	VolumeTitle string // full display title, e.g. "Ascendance of a Bookworm Vol. 22"
	Format      string
	ReleaseDate time.Time
	URL         string
	CoverURL    string

	FirstSeenAt time.Time
	UpdatedAt   time.Time
	AlertedAt   *time.Time
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// NormalizedTitle produces the stable dedupe key component for a title:
// lowercase, alphanumeric-only, single-space separated. Punctuation and
// spacing differences between scrape runs must not create duplicate rows.
func NormalizedTitle(title string) string {
	s := strings.ToLower(title)
	s = nonAlnum.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// DateOnly truncates t to midnight UTC, the canonical storage form for
// release dates.
func DateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
