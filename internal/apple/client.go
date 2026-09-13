// Package apple wraps the public apple.com storefront endpoints that power the
// "check availability" and "delivery date" widgets on Apple's own buy pages.
package apple

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	baseURL = "https://www.apple.com"

	// Requests are serialised. Apple's budget appears to be cumulative rather
	// than per-burst, so concurrency buys nothing and only makes overshoot
	// harder to detect.
	defaultConcurrency = 1

	// Pacing is an AIMD controller, the same shape as TCP congestion control,
	// because the safe rate is not a constant we can hard-code: it depends on
	// how much of apple.com's budget this IP has already spent, which is
	// invisible from outside. Starting slow and easing up only while requests
	// keep succeeding converges on a sustainable rate without having to
	// discover the limit by tripping it.
	//
	// startInterval is deliberately timid: ~0.6 requests/second.
	startInterval = 1600 * time.Millisecond
	// minInterval caps how fast the controller may ever get.
	minInterval = 700 * time.Millisecond
	// maxInterval is the floor on throughput: ~0.2 requests/second.
	maxInterval = 5 * time.Second
	// speedUpStep shaves this off the interval after each clean request.
	speedUpStep = 15 * time.Millisecond
	// slowdownFactor widens the interval when apple.com starts shedding.
	slowdownFactor = 2.0

	maxAttempts = 3
)

// Client talks to apple.com. It is safe for concurrent use.
//
// Apple tolerates short bursts but answers sustained parallel load by shedding
// it, so requests pass through a concurrency gate, a token bucket, and a
// circuit breaker that halts a sweep as soon as apple.com starts refusing.
type Client struct {
	http    *http.Client
	sem     chan struct{}
	breaker *breaker
	// baseURL is overridable so tests can point the client at a stub.
	baseURL string

	mu       sync.Mutex
	nextSlot time.Time
	interval time.Duration
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithBaseURL points the client at another origin — a test stub, or a
// different Apple storefront.
func WithBaseURL(u string) ClientOption {
	return func(c *Client) { c.baseURL = strings.TrimSuffix(u, "/") }
}

// WithRateLimit overrides the pacing between requests. Lower it only against a
// stub; apple.com blocks callers that exceed roughly four per second.
func WithRateLimit(interval time.Duration) ClientOption {
	return func(c *Client) { c.interval = interval }
}

func NewClient(concurrency int, opts ...ClientOption) *Client {
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	c := &Client{
		http: &http.Client{
			Timeout: 25 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        200,
				MaxIdleConnsPerHost: 64,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		sem:      make(chan struct{}, concurrency),
		breaker:  newBreaker(),
		baseURL:  baseURL,
		interval: startInterval,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// reserve hands out evenly spaced send slots, smoothing the whole process's
// request rate regardless of how many goroutines are sweeping.
func (c *Client) reserve() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.nextSlot.Before(now) {
		c.nextSlot = now
	}
	wait := c.nextSlot.Sub(now)
	c.nextSlot = c.nextSlot.Add(c.interval)
	return wait
}

// slowDown halves the request rate. Backing off hard is cheap; overshooting
// costs a multi-minute block of the whole IP.
func (c *Client) slowDown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := time.Duration(float64(c.interval) * slowdownFactor)
	if next > maxInterval {
		next = maxInterval
	}
	c.interval = next
}

// speedUp eases the interval down after a request apple.com served happily.
// Additive, so recovering the rate takes far longer than giving it up.
func (c *Client) speedUp() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.interval <= minInterval {
		return
	}
	c.interval -= speedUpStep
	if c.interval < minInterval {
		c.interval = minInterval
	}
}

// ResetPace is a no-op kept for callers that finish a clean sweep; the
// controller already eases up on its own, and resetting outright would throw
// away what it has learned.
func (c *Client) ResetPace() {}

// Throttled reports whether apple.com is currently refusing this client.
func (c *Client) Throttled() bool {
	return c.breaker.check() > 0
}

// Pace reports the current gap between requests.
func (c *Client) Pace() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.interval
}

func (c *Client) pace(ctx context.Context) error {
	wait := c.reserve()
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) acquire(ctx context.Context) error {
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) release() { <-c.sem }

// get fetches a path, retrying transient failures with jittered backoff.
// Apple answers overload with a 541, which is retryable.
func (c *Client) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	if wait := c.breaker.check(); wait > 0 {
		return nil, &ThrottleError{RetryAfter: wait}
	}
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.release()

	if err := c.pace(ctx); err != nil {
		return nil, err
	}

	u := c.baseURL + path
	if len(q) > 0 {
		// Apple's part numbers contain "/" and the endpoint rejects it escaped
		// as %2F, so the query is assembled by hand rather than via Encode.
		u += "?" + encodeLoose(q)
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := time.Duration(400*(1<<attempt))*time.Millisecond +
				time.Duration(rand.Intn(400))*time.Millisecond
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		//nolint:errcheck // body is drained below via ReadAll
		body, err := io.ReadAll(io.LimitReader(resp.Body, 12<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusOK {
			c.breaker.recordSuccess()
			c.speedUp()
			return body, nil
		}
		lastErr = fmt.Errorf("apple: %s returned %d", path, resp.StatusCode)
		if !isThrottleStatus(resp.StatusCode) && resp.StatusCode < 500 {
			return nil, lastErr
		}
		if isThrottleStatus(resp.StatusCode) {
			if c.breaker.recordThrottle() {
				// Being shed is evidence the current pace is too fast for
				// whatever budget apple.com is applying right now.
				c.slowDown()
			}
			// Once the breaker trips, abandon this request rather than
			// spending its remaining retries on a server already refusing us.
			if wait := c.breaker.check(); wait > 0 {
				return nil, &ThrottleError{RetryAfter: wait}
			}
		}
	}
	return nil, lastErr
}

func (c *Client) getJSON(ctx context.Context, path string, q url.Values, dst any) error {
	body, err := c.get(ctx, path, q)
	if err != nil {
		return err
	}
	if len(body) > 0 && body[0] == '<' {
		return fmt.Errorf("apple: %s returned HTML, not JSON", path)
	}
	return json.Unmarshal(body, dst)
}

// encodeLoose builds a query string leaving "/" unescaped, which the
// fulfillment endpoints require for part numbers like "MDH74LL/A".
func encodeLoose(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	first := true
	for _, k := range keys {
		vs := q[k]
		for _, v := range vs {
			if !first {
				b.WriteByte('&')
			}
			first = false
			b.WriteString(url.QueryEscape(k))
			b.WriteByte('=')
			b.WriteString(strings.ReplaceAll(url.QueryEscape(v), "%2F", "/"))
		}
	}
	return b.String()
}
