// Package fetch provides the shared, polite HTTP client handed to every
// source plugin. Politeness (identifying User-Agent, minimum delay
// between requests, timeouts, retry with backoff) is enforced here so
// individual plugins cannot accidentally hammer a publisher.
package fetch

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Options configures a Client.
type Options struct {
	UserAgent string
	MinDelay  time.Duration
	Timeout   time.Duration
	// MaxRetries is the number of retries after the first attempt for
	// 429/5xx/network errors.
	MaxRetries int
	// Logger, when set, gets one line per request — the scrape
	// progress report in the logs. Defaults to slog.Default().
	Logger *slog.Logger
	// HostDelays overrides MinDelay per hostname, for sites whose
	// robots.txt demands a larger Crawl-delay (e.g. viz.com: 2s).
	// Only overrides that exceed MinDelay take effect.
	HostDelays map[string]time.Duration
}

// Client is a rate-limited HTTP client shared by all source plugins.
type Client struct {
	http       *http.Client
	userAgent  string
	minDelay   time.Duration
	hostDelays map[string]time.Duration
	retries    int
	log        *slog.Logger

	mu      sync.Mutex
	lastReq time.Time
}

// New builds a Client from options, applying sane defaults.
func New(opts Options) *Client {
	if opts.UserAgent == "" {
		opts.UserAgent = "ln-release-bot/1.0"
	}
	if opts.MinDelay <= 0 {
		opts.MinDelay = 1500 * time.Millisecond
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.MaxRetries < 0 {
		opts.MaxRetries = 0
	} else if opts.MaxRetries == 0 {
		opts.MaxRetries = 2
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{
		http:       &http.Client{Timeout: opts.Timeout},
		userAgent:  opts.UserAgent,
		minDelay:   opts.MinDelay,
		hostDelays: opts.HostDelays,
		retries:    opts.MaxRetries,
		log:        opts.Logger,
	}
}

// Get fetches url and returns the response body, retrying transient
// failures. The caller decodes; keeping decoding out of the client lets
// plugins use whatever schema types they need.
func (c *Client) Get(ctx context.Context, url string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt) * 5 * time.Second
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		start := time.Now()
		body, retryable, err := c.do(ctx, url)
		if err == nil {
			c.log.Info("fetch", "url", url, "bytes", len(body),
				"elapsed", time.Since(start).Round(time.Millisecond))
			return body, nil
		}
		c.log.Warn("fetch failed", "url", url, "attempt", attempt+1,
			"retryable", retryable, "err", err)
		lastErr = err
		if !retryable {
			break
		}
	}
	return nil, lastErr
}

func (c *Client) do(ctx context.Context, rawURL string) (body []byte, retryable bool, err error) {
	c.throttle(ctx, rawURL)
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json, text/html;q=0.9, */*;q=0.8")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("GET %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20)) // 32 MiB cap
		if err != nil {
			return nil, true, fmt.Errorf("GET %s: read body: %w", rawURL, err)
		}
		return b, false, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("GET %s: status %d", rawURL, resp.StatusCode)
	default:
		return nil, false, fmt.Errorf("GET %s: status %d", rawURL, resp.StatusCode)
	}
}

// delayFor returns the effective minimum delay before a request to the
// host of rawURL.
func (c *Client) delayFor(rawURL string) time.Duration {
	if u, err := url.Parse(rawURL); err == nil {
		if d, ok := c.hostDelays[strings.TrimPrefix(u.Hostname(), "www.")]; ok && d > c.minDelay {
			return d
		}
		if d, ok := c.hostDelays[u.Hostname()]; ok && d > c.minDelay {
			return d
		}
	}
	return c.minDelay
}

// throttle blocks until the effective delay has passed since the
// previous request (across all plugins sharing this client).
func (c *Client) throttle(ctx context.Context, rawURL string) {
	delay := c.delayFor(rawURL)
	c.mu.Lock()
	wait := delay - time.Since(c.lastReq)
	c.lastReq = time.Now().Add(max(wait, 0))
	c.mu.Unlock()
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
}
