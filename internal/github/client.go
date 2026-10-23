// Package github is a minimal, strictly sequential GitHub REST client that
// authenticates as a GitHub App installation and revalidates every GET with
// ETags, so unchanged resources cost a 304 instead of a fresh body.
package github

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxBody caps a single response body. The largest payloads here (a page of
// workflow runs) are a few hundred KiB.
const maxBody = 16 << 20

// DefaultBaseURL is the public GitHub REST API.
const DefaultBaseURL = "https://api.github.com"

// Options tune a Client. The zero value is usable.
type Options struct {
	// BaseURL of the REST API. Defaults to DefaultBaseURL.
	BaseURL string
	// HTTPClient used for every request. Defaults to a client with a 30s
	// timeout.
	HTTPClient *http.Client
	// UserAgent sent with every request.
	UserAgent string
	// OnRateLimit, if set, receives x-ratelimit-remaining from every API
	// response that carries it.
	OnRateLimit func(remaining float64)
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Stats counts HTTP requests made since the last TakeStats.
type Stats struct {
	// Requests is every HTTP request sent, token minting included.
	Requests int
	// NotModified is how many of those were answered 304 from the ETag cache.
	NotModified int
}

// Response is a successful GET.
type Response struct {
	Body []byte
	// NotModified is true when GitHub answered 304 and Body came from the
	// ETag cache.
	NotModified bool
	// Next is the absolute URL of the next page (Link rel="next"), or "".
	Next string
}

type cacheEntry struct {
	etag string
	body []byte
	next string
	gen  uint64
}

// Client is safe for concurrent use, but serializes every request: GitHub's
// secondary rate limits punish concurrent bursts.
type Client struct {
	base           *url.URL
	hc             *http.Client
	userAgent      string
	onRateLimit    func(float64)
	now            func() time.Time
	appID          int64
	installationID int64
	key            *rsa.PrivateKey

	mu       sync.Mutex
	token    string
	tokenExp time.Time
	cache    map[string]*cacheEntry
	gen      uint64
	stats    Stats
}

// New returns a Client for the given App installation. keyPEM is the App's
// private key (PKCS#1 or PKCS#8).
func New(appID, installationID int64, keyPEM []byte, opts Options) (*Client, error) {
	key, err := ParsePrivateKey(keyPEM)
	if err != nil {
		return nil, err
	}
	base := opts.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid GitHub API base URL %q", base)
	}
	c := &Client{
		base:           u,
		hc:             opts.HTTPClient,
		userAgent:      opts.UserAgent,
		onRateLimit:    opts.OnRateLimit,
		now:            opts.Now,
		appID:          appID,
		installationID: installationID,
		key:            key,
		cache:          map[string]*cacheEntry{},
	}
	if c.hc == nil {
		c.hc = &http.Client{Timeout: 30 * time.Second}
	}
	if c.userAgent == "" {
		c.userAgent = "gh-workflow-exporter"
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// BeginPoll starts a new cache generation. Entries not used between
// BeginPoll and the matching EndPoll are evicted by EndPoll, which keeps the
// ETag cache bounded to what the exporter still polls.
func (c *Client) BeginPoll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
}

// EndPoll evicts cache entries untouched since BeginPoll.
func (c *Client) EndPoll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.cache {
		if e.gen != c.gen {
			delete(c.cache, k)
		}
	}
}

// TakeStats returns the request counters and resets them.
func (c *Client) TakeStats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	c.stats = Stats{}
	return s
}

// Get fetches pathOrURL — an API path such as "/installation/repositories"
// (query allowed) or an absolute next-page URL on the same host — sending
// If-None-Match when a cached ETag exists. A 401 drops the cached token and
// retries once with a fresh one.
func (c *Client) Get(ctx context.Context, pathOrURL string) (*Response, error) {
	u, err := c.resolve(pathOrURL)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for attempt := 0; ; attempt++ {
		res, err := c.getLocked(ctx, u)
		var e *Error
		if attempt == 0 && errors.As(err, &e) && e.Status == http.StatusUnauthorized && !e.mint {
			c.invalidateTokenLocked()
			continue
		}
		return res, err
	}
}

func (c *Client) resolve(pathOrURL string) (string, error) {
	ref, err := url.Parse(pathOrURL)
	if err != nil {
		return "", &Error{Kind: KindParse, Op: "GET " + pathOrURL, Err: err}
	}
	if ref.IsAbs() {
		// Never send the installation token to another host.
		if ref.Scheme != c.base.Scheme || ref.Host != c.base.Host {
			return "", &Error{Kind: KindParse, Op: "GET " + pathOrURL, Err: errors.New("next-page URL points off the API host")}
		}
		return ref.String(), nil
	}
	u := *c.base
	u.Path = c.base.Path + "/" + strings.TrimLeft(ref.Path, "/")
	u.RawQuery = ref.RawQuery
	return u.String(), nil
}

func (c *Client) getLocked(ctx context.Context, u string) (*Response, error) {
	op := "GET " + redactURL(u)
	tok, err := c.tokenLocked(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, &Error{Kind: KindNetwork, Op: op, Err: err}
	}
	c.setHeaders(req, tok)
	entry := c.cache[u]
	if entry != nil {
		req.Header.Set("If-None-Match", entry.etag)
	}
	c.stats.Requests++
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, &Error{Kind: KindNetwork, Op: op, Err: err}
	}
	defer res.Body.Close() //nolint:errcheck // body fully read below
	c.observeRateLimit(res.Header)
	body, err := readBody(res.Body)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			e.Op = op
			return nil, e
		}
		return nil, &Error{Kind: KindNetwork, Op: op, Err: err}
	}

	switch res.StatusCode {
	case http.StatusOK:
		next := nextLink(res.Header.Get("Link"))
		if etag := res.Header.Get("ETag"); etag != "" {
			c.cache[u] = &cacheEntry{etag: etag, body: body, next: next, gen: c.gen}
		} else {
			delete(c.cache, u)
		}
		return &Response{Body: body, Next: next}, nil
	case http.StatusNotModified:
		if entry == nil {
			return nil, &Error{Kind: KindParse, Status: res.StatusCode, Op: op, Err: errors.New("304 without a cached response")}
		}
		c.stats.NotModified++
		entry.gen = c.gen
		return &Response{Body: entry.body, Next: entry.next, NotModified: true}, nil
	default:
		return nil, &Error{Kind: kindForStatus(res.StatusCode), Status: res.StatusCode, Op: op, Err: errors.New(apiMessage(body))}
	}
}

func (c *Client) setHeaders(req *http.Request, bearer string) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Authorization", "Bearer "+bearer)
}

func (c *Client) observeRateLimit(h http.Header) {
	if c.onRateLimit == nil {
		return
	}
	v := h.Get("X-RateLimit-Remaining")
	if v == "" {
		return
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		c.onRateLimit(n)
	}
}

// readBody reads a whole response body, failing (as KindParse) rather than
// truncating when it exceeds maxBody, so a cut-off body is never cached.
func readBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, &Error{Kind: KindParse, Err: fmt.Errorf("response body exceeds %d bytes", maxBody)}
	}
	return body, nil
}

// nextLink extracts the rel="next" URL from an RFC 8288 Link header.
func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		target := strings.TrimSpace(segs[0])
		if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
			continue
		}
		for _, p := range segs[1:] {
			if strings.ReplaceAll(strings.TrimSpace(p), " ", "") == `rel="next"` {
				return target[1 : len(target)-1]
			}
		}
	}
	return ""
}

// apiMessage pulls GitHub's error "message" out of a response body.
func apiMessage(body []byte) string {
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &m) == nil && m.Message != "" {
		return m.Message
	}
	return "unexpected response"
}

// redactURL drops the scheme and host so log lines stay short; API URLs carry
// no credentials.
func redactURL(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return u
	}
	return p.RequestURI()
}

// Decode unmarshals a JSON API body, classifying failures as KindParse.
func Decode(body []byte, v any, op string) error {
	if err := json.Unmarshal(body, v); err != nil {
		return &Error{Kind: KindParse, Op: op, Err: err}
	}
	return nil
}
