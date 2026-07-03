// Package fetch provides the shared, polite HTTP client handed to every
// source plugin. Politeness (identifying User-Agent, minimum delay
// between requests, timeouts, retry with backoff) is enforced here so
// individual plugins cannot accidentally hammer a publisher.
package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
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
	// BrowserTLSHosts are hostnames whose Cloudflare protection gates on
	// TLS fingerprint (JA3) and thus require a browser-shaped TLS
	// handshake (e.g. sevenseasentertainment.com). Requests to these
	// hosts use a Chrome TLS profile; the honest User-Agent is kept.
	BrowserTLSHosts []string
	// FlareSolverrURL, when set, is a FlareSolverr instance (headless
	// Chrome) used to solve Cloudflare *managed challenges* — which a TLS
	// fingerprint alone can't pass (notably from datacenter IPs).
	FlareSolverrURL string
	// FlareSolverrHosts route through FlareSolverr when FlareSolverrURL is
	// set; otherwise those hosts fall back to the browser-TLS path.
	FlareSolverrHosts []string
}

// Client is a rate-limited HTTP client shared by all source plugins.
type Client struct {
	http       *http.Client
	userAgent  string
	minDelay   time.Duration
	hostDelays map[string]time.Duration
	retries    int
	log        *slog.Logger

	timeout      time.Duration
	browserHosts map[string]bool
	browser      tlsclient.HttpClient // lazily built; guarded by browserOnce
	browserOnce  sync.Once
	browserErr   error

	flareURL   string
	flareHosts map[string]bool
	flareHTTP  *http.Client // longer timeout: solving a challenge is slow

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
	browserHosts := map[string]bool{}
	for _, h := range opts.BrowserTLSHosts {
		browserHosts[strings.TrimPrefix(h, "www.")] = true
	}
	flareHosts := map[string]bool{}
	for _, h := range opts.FlareSolverrHosts {
		flareHosts[strings.TrimPrefix(h, "www.")] = true
	}
	return &Client{
		http:         &http.Client{Timeout: opts.Timeout},
		userAgent:    opts.UserAgent,
		minDelay:     opts.MinDelay,
		hostDelays:   opts.HostDelays,
		retries:      opts.MaxRetries,
		log:          opts.Logger,
		browserHosts: browserHosts,
		timeout:      opts.Timeout,
		flareURL:     strings.TrimRight(opts.FlareSolverrURL, "/"),
		flareHosts:   flareHosts,
		flareHTTP:    &http.Client{Timeout: 90 * time.Second},
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

	if c.useFlare(rawURL) {
		return c.doFlare(ctx, rawURL)
	}
	if c.useBrowser(rawURL) {
		return c.doBrowser(ctx, rawURL)
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
	return handleStatus(rawURL, resp.StatusCode, resp.Body)
}

func handleStatus(rawURL string, status int, body io.Reader) ([]byte, bool, error) {
	switch {
	case status == http.StatusOK:
		b, err := io.ReadAll(io.LimitReader(body, 32<<20)) // 32 MiB cap
		if err != nil {
			return nil, true, fmt.Errorf("GET %s: read body: %w", rawURL, err)
		}
		return b, false, nil
	case status == http.StatusTooManyRequests || status >= 500 ||
		status == http.StatusAccepted:
		// 202 from Cloudflare means "challenge in progress"; a retry
		// with the warmed cookie jar often clears it.
		return nil, true, fmt.Errorf("GET %s: status %d", rawURL, status)
	default:
		return nil, false, fmt.Errorf("GET %s: status %d", rawURL, status)
	}
}

func (c *Client) useBrowser(rawURL string) bool {
	if len(c.browserHosts) == 0 {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return c.browserHosts[strings.TrimPrefix(u.Hostname(), "www.")]
}

func (c *Client) useFlare(rawURL string) bool {
	if c.flareURL == "" || len(c.flareHosts) == 0 {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return c.flareHosts[strings.TrimPrefix(u.Hostname(), "www.")]
}

// doFlare fetches through a FlareSolverr instance, which drives headless
// Chrome to solve the Cloudflare challenge and returns the rendered page.
func (c *Client) doFlare(ctx context.Context, targetURL string) ([]byte, bool, error) {
	reqBody, err := json.Marshal(map[string]any{
		"cmd":        "request.get",
		"url":        targetURL,
		"maxTimeout": 60000,
	})
	if err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.flareURL+"/v1", bytes.NewReader(reqBody))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.flareHTTP.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("flaresolverr GET %s: %w", targetURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, true, fmt.Errorf("flaresolverr GET %s: proxy status %d", targetURL, resp.StatusCode)
	}

	var fr struct {
		Status   string `json:"status"`
		Message  string `json:"message"`
		Solution struct {
			Status   int    `json:"status"`
			Response string `json:"response"`
		} `json:"solution"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, true, fmt.Errorf("flaresolverr GET %s: read: %w", targetURL, err)
	}
	if err := json.Unmarshal(body, &fr); err != nil {
		return nil, true, fmt.Errorf("flaresolverr GET %s: decode: %w", targetURL, err)
	}
	if fr.Status != "ok" {
		return nil, true, fmt.Errorf("flaresolverr GET %s: %s", targetURL, fr.Message)
	}
	return handleStatus(targetURL, fr.Solution.Status, strings.NewReader(fr.Solution.Response))
}

// doBrowser issues the request with a Chrome TLS fingerprint. The
// User-Agent stays honest — only the TLS handshake is browser-shaped,
// which is what Cloudflare's JA3 gate keys on.
func (c *Client) doBrowser(ctx context.Context, rawURL string) ([]byte, bool, error) {
	c.browserOnce.Do(func() {
		secs := int(c.timeout.Seconds())
		if secs <= 0 {
			secs = 30
		}
		c.browser, c.browserErr = tlsclient.NewHttpClient(
			tlsclient.NewNoopLogger(),
			tlsclient.WithClientProfile(profiles.Chrome_133),
			tlsclient.WithTimeoutSeconds(secs),
			tlsclient.WithCookieJar(tlsclient.NewCookieJar()),
		)
	})
	if c.browserErr != nil {
		return nil, false, fmt.Errorf("browser client init: %w", c.browserErr)
	}

	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header = fhttp.Header{
		"accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
		"accept-language":           {"en-US,en;q=0.9"},
		"user-agent":                {c.userAgent},
		"sec-fetch-dest":            {"document"},
		"sec-fetch-mode":            {"navigate"},
		"sec-fetch-site":            {"none"},
		"upgrade-insecure-requests": {"1"},
		fhttp.HeaderOrderKey: {
			"accept", "accept-language", "user-agent",
			"sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site",
			"upgrade-insecure-requests",
		},
	}
	resp, err := c.browser.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("GET %s (browser): %w", rawURL, err)
	}
	defer resp.Body.Close()
	return handleStatus(rawURL, resp.StatusCode, resp.Body)
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
