package request

import (
	"net/http"
	"testing"
	"time"
)

func TestRetryAfterBackoffHonorsAndCapsRetryAfter(t *testing.T) {
	t.Parallel()
	const maxWait = 30 * time.Second
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"5", 5 * time.Second},
		{"600", maxWait},
		// 9223372037 s overflows int64 nanoseconds; it used to wrap negative
		// and skip the wait entirely.
		{"9223372037", maxWait},
		{"99999999999999999", maxWait},
	} {
		resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
		resp.Header.Set("Retry-After", tc.header)
		if got := retryAfterBackoff(time.Second, maxWait, 1, resp); got != tc.want {
			t.Errorf("Retry-After %s: wait = %v, want %v", tc.header, got, tc.want)
		}
	}
}
