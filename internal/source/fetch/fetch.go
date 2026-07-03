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
	// CFScannerAccount / CFScannerToken enable fetching via the Cloudflare
	// URL Scanner API: Cloudflare renders the page from its own infra,
	// which defeats IP-based anti-bot challenges (e.g. SiteGround's, which
	// challenge datacenter IPs). Both must be set to activate.
	CFScannerAccount string
	CFScannerToken   string
	// CFScannerHosts route through the URL Scanner when it's configured;
	// otherwise those hosts fall back to the browser-TLS path.
	CFScannerHosts []string
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

	cfAPI      string // "" when URL Scanner is not configured
	cfToken    string
	cfHosts    map[string]bool
	cfClient   *http.Client  // for URL Scanner API calls
	cfPollWait time.Duration // between result polls (overridable in tests)

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
	cfHosts := map[string]bool{}
	for _, h := range opts.CFScannerHosts {
		cfHosts[strings.TrimPrefix(h, "www.")] = true
	}
	cfAPI := ""
	if opts.CFScannerAccount != "" && opts.CFScannerToken != "" {
		cfAPI = "https://api.cloudflare.com/client/v4/accounts/" +
			opts.CFScannerAccount + "/urlscanner/v2"
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
		cfAPI:        cfAPI,
		cfToken:      opts.CFScannerToken,
		cfHosts:      cfHosts,
		cfClient:     &http.Client{Timeout: 30 * time.Second},
		cfPollWait:   cfPollInterval,
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

	if c.useCFScanner(rawURL) {
		return c.doCFScan(ctx, rawURL)
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

func (c *Client) useCFScanner(rawURL string) bool {
	if c.cfAPI == "" || len(c.cfHosts) == 0 {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return c.cfHosts[strings.TrimPrefix(u.Hostname(), "www.")]
}

// cfScan polling parameters: scans typically finish in ~30-90s.
const (
	cfPollInterval = 8 * time.Second
	cfMaxPolls     = 20
)

// doCFScan fetches a URL via the Cloudflare URL Scanner: it creates a
// scan (Cloudflare renders the page from its own infrastructure, running
// any JS anti-bot challenge), polls until the scan completes, then
// returns the captured main-document HTML.
func (c *Client) doCFScan(ctx context.Context, targetURL string) ([]byte, bool, error) {
	uuid, retryable, err := c.cfCreateScan(ctx, targetURL)
	if err != nil {
		return nil, retryable, err
	}

	for poll := 0; poll < cfMaxPolls; poll++ {
		select {
		case <-time.After(c.cfPollWait):
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
		hash, done, err := c.cfPollResult(ctx, uuid)
		if err != nil {
			return nil, true, fmt.Errorf("cfscan %s: %w", targetURL, err)
		}
		if !done {
			continue
		}
		if hash == "" {
			return nil, true, fmt.Errorf("cfscan %s: no captured response", targetURL)
		}
		return c.cfFetchResponse(ctx, targetURL, hash)
	}
	return nil, true, fmt.Errorf("cfscan %s: scan did not finish in %s",
		targetURL, time.Duration(cfMaxPolls)*cfPollInterval)
}

func (c *Client) cfDo(ctx context.Context, method, url string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.cfClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return resp.StatusCode, b, err
}

func (c *Client) cfCreateScan(ctx context.Context, targetURL string) (uuid string, retryable bool, err error) {
	reqBody, _ := json.Marshal(map[string]string{"url": targetURL})
	status, body, err := c.cfDo(ctx, http.MethodPost, c.cfAPI+"/scan", reqBody)
	if err != nil {
		return "", true, fmt.Errorf("cfscan create %s: %w", targetURL, err)
	}
	switch status {
	case http.StatusOK, http.StatusAccepted:
		var r struct {
			UUID string `json:"uuid"`
		}
		if err := json.Unmarshal(body, &r); err != nil || r.UUID == "" {
			return "", true, fmt.Errorf("cfscan create %s: bad response: %s", targetURL, truncate(body))
		}
		return r.UUID, false, nil
	case http.StatusConflict:
		// A recent scan of this URL exists; reuse the newest.
		return c.cfSearchRecent(ctx, targetURL)
	default:
		return "", true, fmt.Errorf("cfscan create %s: status %d: %s", targetURL, status, truncate(body))
	}
}

// cfSearchRecent finds the most recent existing scan for a URL (used when
// creation 409s due to Cloudflare's dedup window).
func (c *Client) cfSearchRecent(ctx context.Context, targetURL string) (string, bool, error) {
	q := url.Values{"url": {targetURL}, "size": {"1"}}
	status, body, err := c.cfDo(ctx, http.MethodGet, c.cfAPI+"/search?"+q.Encode(), nil)
	if err != nil {
		return "", true, fmt.Errorf("cfscan search %s: %w", targetURL, err)
	}
	if status != http.StatusOK {
		return "", true, fmt.Errorf("cfscan search %s: status %d", targetURL, status)
	}
	var r struct {
		Tasks []struct {
			UUID string `json:"uuid"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(body, &r); err != nil || len(r.Tasks) == 0 {
		return "", true, fmt.Errorf("cfscan search %s: no prior scan", targetURL)
	}
	return r.Tasks[0].UUID, false, nil
}

// cfPollResult checks a scan. done is false while it's still processing
// (the result endpoint 404s until ready); when done, hash is the main
// document's response-body hash (the first captured response).
func (c *Client) cfPollResult(ctx context.Context, uuid string) (hash string, done bool, err error) {
	status, body, err := c.cfDo(ctx, http.MethodGet, c.cfAPI+"/result/"+uuid, nil)
	if err != nil {
		return "", false, err
	}
	if status == http.StatusNotFound {
		return "", false, nil // still processing
	}
	if status != http.StatusOK {
		return "", false, fmt.Errorf("result status %d: %s", status, truncate(body))
	}
	var r struct {
		Task struct {
			Success bool `json:"success"`
		} `json:"task"`
		Lists struct {
			Hashes []string `json:"hashes"`
		} `json:"lists"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", false, fmt.Errorf("decode result: %w", err)
	}
	if !r.Task.Success {
		return "", false, fmt.Errorf("scan reported failure")
	}
	if len(r.Lists.Hashes) == 0 {
		return "", true, nil
	}
	return r.Lists.Hashes[0], true, nil
}

func (c *Client) cfFetchResponse(ctx context.Context, targetURL, hash string) ([]byte, bool, error) {
	status, body, err := c.cfDo(ctx, http.MethodGet, c.cfAPI+"/responses/"+hash, nil)
	if err != nil {
		return nil, true, fmt.Errorf("cfscan body %s: %w", targetURL, err)
	}
	if status != http.StatusOK {
		return nil, true, fmt.Errorf("cfscan body %s: status %d", targetURL, status)
	}
	return body, false, nil
}

func truncate(b []byte) string {
	const n = 200
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
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
