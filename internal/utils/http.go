package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.uber.org/ratelimit"
)

// rateLimitSlackDivisor gives rate limiters a slack of 10% of their rate.
const rateLimitSlackDivisor = 10

// ParseRateLimit parses "<count>/<unit>" (second, minute, hour, day) into a
// limiter, or returns nil for an empty or invalid spec.
func ParseRateLimit(rateStr string) ratelimit.Limiter {
	countStr, unitStr, ok := strings.Cut(rateStr, "/")
	if !ok {
		return nil
	}

	count, err := strconv.Atoi(strings.TrimSpace(countStr))
	if err != nil || count <= 0 {
		return nil
	}
	slackSize := count / rateLimitSlackDivisor

	unit := strings.ToLower(strings.TrimSpace(unitStr))
	unit = strings.TrimSuffix(unit, "s")
	switch unit {
	case "minute", "min":
		return ratelimit.New(count, ratelimit.Per(time.Minute), ratelimit.WithSlack(slackSize))
	case "second", "sec":
		return ratelimit.New(count, ratelimit.Per(time.Second), ratelimit.WithSlack(slackSize))
	case "hour", "hr":
		return ratelimit.New(count, ratelimit.Per(time.Hour), ratelimit.WithSlack(slackSize))
	case "day", "d":
		return ratelimit.New(count, ratelimit.Per(day), ratelimit.WithSlack(slackSize))
	default:
		return nil
	}
}

// JSONResponse writes data as indented JSON with the given status code.
func JSONResponse(w http.ResponseWriter, data any, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if data != nil {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(data)
	}
}

// ValidateURL accepts an http(s) URL or a bare host:port.
func ValidateURL(urlStr string) error {
	if urlStr == "" {
		return fmt.Errorf("URL cannot be empty")
	}

	// Try parsing as full URL first
	u, err := url.Parse(urlStr)
	if err == nil && u.Scheme != "" && u.Host != "" {
		// It's a full URL, validate scheme
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("URL scheme must be http or https")
		}
		return nil
	}

	// Check if it's a host:port format (no scheme)
	if strings.Contains(urlStr, ":") && !strings.Contains(urlStr, "://") {
		// Try parsing with http:// prefix
		u, err = url.Parse("http://" + urlStr)
		if err != nil {
			return fmt.Errorf("invalid host:port format: %w", err)
		}

		if u.Host == "" {
			return fmt.Errorf("host is required in host:port format")
		}

		// Validate port number
		if u.Port() == "" {
			return fmt.Errorf("port is required in host:port format")
		}

		return nil
	}

	return fmt.Errorf("invalid URL format: %s", urlStr)
}

// JoinURL joins paths onto base. A query string on the last path element is
// kept verbatim after the joined path.
func JoinURL(base string, paths ...string) (string, error) {
	if len(paths) == 0 {
		return url.JoinPath(base)
	}
	// Copy so the caller's slice is not rewritten, and split only at the
	// first '?' so a query containing '?' survives intact.
	paths = append([]string(nil), paths...)
	last, query, hasQuery := strings.Cut(paths[len(paths)-1], "?")
	paths[len(paths)-1] = last

	joined, err := url.JoinPath(base, paths...)
	if err != nil {
		return "", err
	}
	if hasQuery {
		return joined + "?" + query, nil
	}
	return joined, nil
}

// DownloadOptions adjusts a download request before it is sent.
type DownloadOptions func(r *http.Request)

// WithHeader sets a request header on a download.
func WithHeader(key, value string) DownloadOptions {
	return func(r *http.Request) {
		r.Header.Set(key, value)
	}
}

// downloadTimeout bounds a whole NZB or .torrent fetch, so a stalled indexer
// cannot hang the importing request (and its connection) forever.
const downloadTimeout = 5 * time.Minute

// fetch GETs rawURL with a bounded client and returns the response only for
// 200 OK. The caller closes the body.
func fetch(rawURL string, options ...DownloadOptions) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	for _, opt := range options {
		opt(req)
	}

	client := &http.Client{Timeout: downloadTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("status code %d", resp.StatusCode)
	}
	return resp, nil
}

// DownloadFile fetches url and returns the server-suggested filename and body.
func DownloadFile(url string, options ...DownloadOptions) (string, []byte, error) {
	resp, err := fetch(url, options...)
	if err != nil {
		return "", nil, fmt.Errorf("failed to download file: %w", err)
	}
	defer resp.Body.Close()

	filename := getFilenameFromResponse(resp, url)

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return filename, data, nil
}

func getFilenameFromResponse(resp *http.Response, originalURL string) string {
	// 1. Try Content-Disposition header
	if filename := filenameFromDisposition(resp.Header.Get("Content-Disposition")); filename != "" {
		return filename
	}

	// 2. Fall back to URL path
	if parsedURL, err := url.Parse(originalURL); err == nil {
		if filename := filepath.Base(parsedURL.Path); filename != "." && filename != "/" {
			// URL decode the filename
			if decoded, queryUnescapeErr := url.QueryUnescape(filename); queryUnescapeErr == nil {
				return decoded
			}
			return filename
		}
	}

	// 3. Default filename
	return "downloaded_file"
}

// filenameFromDisposition returns the filename a Content-Disposition header
// names, or "" when there is none.
func filenameFromDisposition(cd string) string {
	if cd == "" {
		return ""
	}
	// First try standard MIME parsing
	if _, params, err := mime.ParseMediaType(cd); err == nil {
		// RFC 5987: filename* takes precedence
		if filename := params["filename*"]; filename != "" {
			return filename
		}
		if filename := params["filename"]; filename != "" {
			return filename
		}
	}
	// Manual fallback for non-compliant headers (unquoted filenames with special chars)
	return extractFilenameManual(cd)
}

// extractFilenameManual handles non-compliant Content-Disposition headers
// where filename is not properly quoted (e.g., filename=[Erai-raws]...nzb).
func extractFilenameManual(cd string) string {
	// Try filename*= first (RFC 5987)
	if _, after, ok := strings.Cut(cd, "filename*="); ok {
		value := after
		// Handle UTF-8'' prefix
		if strings.HasPrefix(value, "UTF-8''") || strings.HasPrefix(value, "utf-8''") {
			value = value[7:]
		}
		// Take until semicolon or end
		if semi := strings.Index(value, ";"); semi != -1 {
			value = value[:semi]
		}
		value = strings.Trim(value, `"' `)
		if decoded, err := url.QueryUnescape(value); err == nil {
			return decoded
		}
		return value
	}

	// Try filename= (simple case)
	if _, after, ok := strings.Cut(cd, "filename="); ok {
		value := after
		// Take until semicolon or end
		if semi := strings.Index(value, ";"); semi != -1 {
			value = value[:semi]
		}
		// Remove surrounding quotes if present
		value = strings.Trim(value, `"' `)
		if value != "" {
			return value
		}
	}

	return ""
}

// GetContentType returns the MIME type for fileName's extension, or
// application/octet-stream.
func GetContentType(fileName string) string {
	contentType := mime.TypeByExtension(filepath.Ext(fileName))
	if contentType == "" {
		return "application/octet-stream"
	}
	return contentType
}

// IsValidURL checks if a string is a valid HTTP/HTTPS URL.
// Optimized for speed with early exits before calling [url.Parse].
func IsValidURL(s string) bool {
	if len(s) < len("http://a.b") {
		return false
	}

	// Fast scheme check without allocation
	var host string
	switch {
	case strings.HasPrefix(s, "http://"):
		host = s[len("http://"):]
	case strings.HasPrefix(s, "https://"):
		host = s[len("https://"):]
	default:
		return false
	}

	// Check host portion is non-empty
	if slashIdx := strings.IndexByte(host, '/'); slashIdx != -1 {
		host = host[:slashIdx]
	}
	if len(host) == 0 {
		return false
	}

	// Full parse for edge cases (ports, userinfo, IPv6, etc.)
	u, err := url.Parse(s)
	return err == nil && u.Host != ""
}
