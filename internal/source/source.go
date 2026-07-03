// Package source defines the plugin contract for release sources and the
// registry they self-register into.
//
// Adding a new source:
//
//  1. Create internal/source/<name>/<name>.go implementing source.Source.
//  2. Call source.Register(&yourSource{}) from the package's init().
//  3. Add a blank import for the package in internal/source/all/all.go.
//  4. Enable it in config.yaml under sources:.
package source

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/source/fetch"
)

// Mode selects how much of a source's catalog a Fetch should cover.
type Mode int

const (
	// ModeIncremental covers the near-term window (recent past through
	// the upcoming calendar). Used by the daily scheduled scrape.
	ModeIncremental Mode = iota
	// ModeFull covers the source's entire catalog, past and future.
	// Used by the manual /scrape command (backfill, slip correction).
	ModeFull
)

// Source is a single publisher/site scraped for release data.
//
// Fetch returns the releases the publisher lists for the given mode.
// It must use the provided fetch.Client for all HTTP so politeness is
// centrally enforced, and must honor ctx cancellation. Idempotency and
// deduplication are handled downstream — plugins just report what the
// publisher currently lists.
type Source interface {
	// Name is the unique registry/config/database key, e.g. "jnovelclub".
	Name() string
	// Publisher is the human-readable name, e.g. "J-Novel Club".
	Publisher() string
	// Fetch retrieves the currently listed releases for mode.
	Fetch(ctx context.Context, c *fetch.Client, mode Mode) ([]model.Release, error)
}

var (
	mu       sync.Mutex
	registry = map[string]Source{}
)

// Register adds a Source to the global registry. It is intended to be
// called from plugin init() functions and panics on duplicate names —
// that is a programmer error caught at startup.
func Register(s Source) {
	mu.Lock()
	defer mu.Unlock()
	name := s.Name()
	if name == "" {
		panic("source: Register called with empty name")
	}
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("source: duplicate registration for %q", name))
	}
	registry[name] = s
}

// All returns every registered source, sorted by name.
func All() []Source {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Source, 0, len(registry))
	for _, s := range registry {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Enabled returns registered sources filtered by the given predicate
// (typically config.SourceEnabled), sorted by name.
func Enabled(enabled func(name string) bool) []Source {
	var out []Source
	for _, s := range All() {
		if enabled(s.Name()) {
			out = append(out, s)
		}
	}
	return out
}
