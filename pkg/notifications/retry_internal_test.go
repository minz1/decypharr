package notifications

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	t.Parallel()
	tooMany := &StatusError{Code: http.StatusTooManyRequests}
	unavailable := &StatusError{Code: http.StatusServiceUnavailable, RetryAfter: 12 * time.Second}
	longWait := &StatusError{Code: http.StatusTooManyRequests, RetryAfter: 120 * time.Second}
	network := errors.New("dial tcp: connection refused")
	tests := []struct {
		name    string
		attempt int
		err     error
		retry   bool
		wait    time.Duration
	}{
		{"delivered", 1, nil, false, 0},
		{"permanent", 1, permanent(errors.New("marshal")), false, 0},
		{"429 first", 1, tooMany, true, time.Second},
		{"429 second", 2, tooMany, true, 5 * time.Second},
		{"429 third", 3, tooMany, true, 30 * time.Second},
		{"429 fourth gives up", 4, tooMany, false, 0},
		{"503 retry-after over backoff", 1, unavailable, true, 12 * time.Second},
		{"503 retry-after over 5s", 2, unavailable, true, 12 * time.Second},
		{"503 backoff over retry-after", 3, unavailable, true, 30 * time.Second},
		{"retry-after clamped to 30s", 1, longWait, true, 30 * time.Second},
		{"400 not retried", 1, &StatusError{Code: http.StatusBadRequest}, false, 0},
		{"404 not retried", 1, &StatusError{Code: http.StatusNotFound}, false, 0},
		{"302 not retried", 1, &StatusError{Code: http.StatusFound}, false, 0},
		{"network first", 1, network, true, time.Second},
		{"network third", 3, network, true, 30 * time.Second},
		{"network fourth gives up", 4, network, false, 0},
		{"wrapped network", 2, fmt.Errorf("send: %w", network), true, 5 * time.Second},
		{"canceled", 1, context.Canceled, false, 0},
		{"wrapped canceled", 1, fmt.Errorf("send: %w", context.Canceled), false, 0},
		{"send timeout retried", 1, context.DeadlineExceeded, true, time.Second},
		{
			"send timeout in url error",
			1,
			&url.Error{Op: "Post", URL: "http://x", Err: context.DeadlineExceeded},
			true,
			time.Second,
		},
		{"send timeout fourth gives up", 4, context.DeadlineExceeded, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			retry, wait := decide(tt.attempt, tt.err)
			if retry != tt.retry || wait != tt.wait {
				t.Fatalf("decide(%d, %v) = (%v, %v), want (%v, %v)", tt.attempt, tt.err, retry, wait, tt.retry, tt.wait)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 8, 5, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"12", 12 * time.Second},
		{"1.5", 1500 * time.Millisecond},
		{"", 0},
		{"abc", 0},
		{"-3", 0},
		{now.Add(20 * time.Second).Format(http.TimeFormat), 20 * time.Second},
		{now.Add(-5 * time.Second).Format(http.TimeFormat), 0},
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.in, now); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
