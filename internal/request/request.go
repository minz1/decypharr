package request

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/rs/zerolog"
	"go.uber.org/ratelimit"
	"golang.org/x/net/proxy"
)

// Client defaults.
const (
	defaultMaxRetries     = 5
	defaultTimeout        = 60 * time.Second
	dialTimeout           = 30 * time.Second
	dialKeepAlive         = 15 * time.Second
	maxIdleConns          = 100
	maxIdleConnsPerHost   = 10
	idleConnTimeout       = 30 * time.Second
	responseHeaderTimeout = 30 * time.Second
	expectContinueTimeout = 1 * time.Second
	minRetryWait          = 1 * time.Second
	maxRetryWait          = 30 * time.Second
)

// ClientOption configures a Client in New.
type ClientOption func(*Client)

// Client represents an HTTP client with additional capabilities.
type Client struct {
	client          *retryablehttp.Client
	httpClient      *http.Client // underlying http client
	rateLimiter     ratelimit.Limiter
	headers         map[string]string
	headersMu       sync.RWMutex
	maxRetries      int
	timeout         time.Duration
	tlsConfig       *tls.Config
	retryableStatus map[int]struct{}
	logger          zerolog.Logger
	proxy           string
}

// WithMaxRetries sets the maximum number of retry attempts.
func WithMaxRetries(maxRetries int) ClientOption {
	return func(c *Client) {
		c.maxRetries = maxRetries
	}
}

// WithTimeout sets the request timeout.
func WithTimeout(timeout time.Duration) ClientOption {
	return func(c *Client) {
		c.timeout = timeout
	}
}

// WithRateLimiter sets a rate limiter.
func WithRateLimiter(rl ratelimit.Limiter) ClientOption {
	return func(c *Client) {
		c.rateLimiter = rl
	}
}

// WithHeaders sets default headers.
func WithHeaders(headers map[string]string) ClientOption {
	return func(c *Client) {
		c.headersMu.Lock()
		c.headers = headers
		c.headersMu.Unlock()
	}
}

// SetHeader sets a default header sent with every request.
func (c *Client) SetHeader(key, value string) {
	c.headersMu.Lock()
	c.headers[key] = value
	c.headersMu.Unlock()
}

// WithRetryableStatus adds status codes that should trigger a retry.
func WithRetryableStatus(statusCodes ...int) ClientOption {
	return func(c *Client) {
		c.retryableStatus = make(map[int]struct{}) // reset the map
		for _, code := range statusCodes {
			c.retryableStatus[code] = struct{}{}
		}
	}
}

// WithProxy routes requests through an HTTP(S) or socks5:// proxy.
func WithProxy(proxyURL string) ClientOption {
	return func(c *Client) {
		c.proxy = proxyURL
	}
}

// Do performs an HTTP request with retries for certain status codes.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	// Apply headers
	c.headersMu.RLock()
	if c.headers != nil {
		for key, value := range c.headers {
			req.Header.Set(key, value)
		}
	}
	c.headersMu.RUnlock()

	// Apply rate limiting
	if c.rateLimiter != nil {
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		default:
			c.rateLimiter.Take()
		}
	}

	// Convert to retryablehttp request
	retryReq, err := retryablehttp.FromRequest(req)
	if err != nil {
		return nil, fmt.Errorf("creating retryable request: %w", err)
	}

	return c.client.Do(retryReq)
}

// MakeRequest performs an HTTP request and returns the response body as bytes.
func (c *Client) MakeRequest(req *http.Request) ([]byte, error) {
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}

	defer func() {
		if closeErr := res.Body.Close(); closeErr != nil {
			c.logger.Printf("Failed to close response body: %v", closeErr)
		}
	}()

	bodyBytes, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP error %d: %s", res.StatusCode, string(bodyBytes))
	}

	return bodyBytes, nil
}

// Get issues an unbounded-context GET; prefer Do with a request context.
func (c *Client) Get(url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating GET request: %w", err)
	}

	return c.Do(req)
}

// retryAfterBackoff extends DefaultBackoff with Retry-After header support.
// When a 429 response carries a Retry-After header decypharr waits exactly as
// long as the server requests (capped at maxWait) instead of using jittered
// exponential backoff.
func retryAfterBackoff(minWait, maxWait time.Duration, attemptNum int, resp *http.Response) time.Duration {
	if wait, ok := retryAfter(resp); ok {
		return min(wait, maxWait)
	}
	return retryablehttp.DefaultBackoff(minWait, maxWait, attemptNum, resp)
}

// retryAfter returns the positive wait a 429 response asks for, either as
// delay-seconds or as an HTTP date. Huge second counts saturate instead of
// overflowing into a negative duration.
func retryAfter(resp *http.Response) (time.Duration, bool) {
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(ra, 10, 64); err == nil {
		if secs <= 0 {
			return 0, false
		}
		if secs > int64(math.MaxInt64/time.Second) {
			return math.MaxInt64, true
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(ra); err == nil {
		if wait := time.Until(t); wait > 0 {
			return wait, true
		}
	}
	return 0, false
}

// New creates an HTTP client. logger receives the client's own diagnostics;
// tlsConfig is the verified TLS base for HTTPS (the configured CA file);
// nil means the system roots. Both are required so no caller forgets them.
func New(logger zerolog.Logger, tlsConfig *tls.Config, options ...ClientOption) *Client {
	if tlsConfig == nil {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	client := &Client{
		maxRetries: defaultMaxRetries,
		tlsConfig:  tlsConfig.Clone(),
		retryableStatus: map[int]struct{}{
			http.StatusTooManyRequests:     {},
			http.StatusInternalServerError: {},
			http.StatusBadGateway:          {},
			http.StatusServiceUnavailable:  {},
			http.StatusGatewayTimeout:      {},
		},
		logger:  logger,
		timeout: defaultTimeout,
		proxy:   "",
		headers: make(map[string]string),
	}

	// Create default http client
	client.httpClient = &http.Client{
		Timeout: client.timeout,
	}

	// Apply options before configuring transport
	for _, option := range options {
		option(client)
	}

	client.httpClient.Timeout = client.timeout

	// Check if transport was set by WithTransport option
	if client.httpClient.Transport == nil {
		transport := &http.Transport{
			TLSClientConfig: client.tlsConfig,
			DialContext: (&net.Dialer{
				Timeout:   dialTimeout,
				KeepAlive: dialKeepAlive,
			}).DialContext,
			MaxIdleConns:          maxIdleConns,
			MaxIdleConnsPerHost:   maxIdleConnsPerHost,
			IdleConnTimeout:       idleConnTimeout,
			ResponseHeaderTimeout: responseHeaderTimeout,
			ExpectContinueTimeout: expectContinueTimeout,
			ForceAttemptHTTP2:     true,
		}

		// Configure proxy if needed
		SetProxy(transport, client.proxy)

		// Set the transport to the client
		client.httpClient.Transport = transport
	}

	// Create retryablehttp client
	retryClient := retryablehttp.NewClient()
	retryClient.HTTPClient = client.httpClient
	retryClient.RetryMax = client.maxRetries
	retryClient.RetryWaitMin = minRetryWait
	retryClient.RetryWaitMax = maxRetryWait
	retryClient.Logger = nil
	retryClient.Backoff = retryAfterBackoff

	// Custom retry policy based on retryable status codes
	retryClient.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		// Don't retry on context errors
		if ctx.Err() != nil {
			return false, ctx.Err()
		}

		// Use the default policy for transport errors. HTTP responses use the
		// configured status list so provider errors retain their response body.
		if err != nil || resp == nil {
			return retryablehttp.DefaultRetryPolicy(ctx, resp, err)
		}

		_, retry := client.retryableStatus[resp.StatusCode]
		return retry, nil
	}

	client.client = retryClient

	return client
}

// ParseProxy parses a proxy URL: http://, https://, socks5:// or socks5h://
// (the schemes net/http supports), with a host. A bare host:port means an
// HTTP proxy, as with curl.
func ParseProxy(proxyURL string) (*url.URL, error) {
	raw := strings.TrimSpace(proxyURL)
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %w", err)
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("invalid proxy URL %q: scheme must be http, https, socks5 or socks5h", parsed.Redacted())
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("invalid proxy URL %q: no host", parsed.Redacted())
	}
	return parsed, nil
}

// SetProxy configures transport for proxyURL: socks5:// dials through a
// SOCKS5 proxy, anything else is an HTTP(S) proxy URL, and "" uses the
// environment. An invalid URL fails every request with its parse error
// rather than letting them bypass the configured proxy.
func SetProxy(transport *http.Transport, proxyURL string) {
	if proxyURL == "" {
		transport.Proxy = http.ProxyFromEnvironment
		return
	}
	parsed, err := ParseProxy(proxyURL)
	if err != nil {
		failAllRequests(transport, err)
		return
	}
	// x/net's SOCKS5 dialer passes host names to the proxy to resolve, so it
	// serves socks5h too.
	if parsed.Scheme != "socks5" && parsed.Scheme != "socks5h" {
		transport.Proxy = http.ProxyURL(parsed)
		return
	}

	auth := &proxy.Auth{}
	if parsed.User != nil {
		auth.User = parsed.User.Username()
		auth.Password, _ = parsed.User.Password()
	}
	dialer, err := proxy.SOCKS5("tcp", parsed.Host, auth, proxy.Direct)
	if err != nil {
		failAllRequests(transport, fmt.Errorf("socks5 proxy: %w", err))
		return
	}
	// The x/net SOCKS5 dialer implements ContextDialer; use it so request
	// cancellation also aborts the proxy dial.
	if cd, ok := dialer.(proxy.ContextDialer); ok {
		transport.DialContext = cd.DialContext
		return
	}
	transport.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
		return dialer.Dial(network, addr)
	}
}

// failAllRequests makes every request through transport fail with err.
func failAllRequests(transport *http.Transport, err error) {
	transport.Proxy = func(*http.Request) (*url.URL, error) { return nil, err }
	transport.DialContext = func(context.Context, string, string) (net.Conn, error) { return nil, err }
}
