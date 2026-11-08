package notifications

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const (
	// sendTimeout bounds one send of one event (owner-proxy R2).
	sendTimeout = 10 * time.Second
	// maxSends is the first send and three retries (SM06-4 read by R1).
	maxSends = 4
	// maxRetryAfter caps the wait a server's Retry-After can ask for (R3).
	maxRetryAfter = 30 * time.Second
	// maxErrorBody is how much of an error response is kept for the log.
	maxErrorBody = 1024
)

// StatusError is a response outside 2xx. RetryAfter is the server's
// Retry-After, zero when it sent none or one that could not be read.
type StatusError struct {
	Code       int
	Status     string
	Body       string
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return e.Status
	}
	return e.Status + ": " + e.Body
}

// newStatusError reads the status, the first maxErrorBody bytes of the body
// and the Retry-After of a non-2xx response.
func newStatusError(resp *http.Response, now time.Time) *StatusError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return &StatusError{
		Code:       resp.StatusCode,
		Status:     resp.Status,
		Body:       string(body),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), now),
	}
}

// parseRetryAfter reads a Retry-After value: seconds (whole or decimal) or an
// HTTP-date. Anything unreadable, negative or already past is zero.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs * float64(time.Second))
	}
	at, err := http.ParseTime(v)
	if err != nil {
		return 0
	}
	if d := at.Sub(now); d > 0 {
		return d
	}
	return 0
}

// permanentError is a send that failed before the request left: sending the
// same event again cannot succeed.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func permanent(err error) error { return &permanentError{err: err} }

// The waits before the first, second and third retry (SM06-4).
const (
	firstRetryWait  = time.Second
	secondRetryWait = 5 * time.Second
	thirdRetryWait  = 30 * time.Second
)

// backoff is the wait before the retry that follows send number attempt.
func backoff(attempt int) time.Duration {
	waits := [maxSends - 1]time.Duration{firstRetryWait, secondRetryWait, thirdRetryWait}
	if attempt < 1 || attempt > len(waits) {
		return 0
	}
	return waits[attempt-1]
}

// decide says whether the event whose send number attempt ended with err is
// sent again, and after how long (SM06-4, R1–R4): network errors, 429 and
// 5xx are retried up to maxSends sends; anything else ends the event.
func decide(attempt int, err error) (bool, time.Duration) {
	if err == nil || attempt >= maxSends || !retryable(err) {
		return false, 0
	}
	wait := backoff(attempt)
	if se, ok := errors.AsType[*StatusError](err); ok {
		wait = max(wait, min(se.RetryAfter, maxRetryAfter))
	}
	return true, wait
}

// retryable says whether a failed send is worth sending again at all: a
// network error (a send timeout included), 429 or 5xx. A cancelled send, a
// request that could not be built and any other status are not.
func retryable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if _, ok := errors.AsType[*permanentError](err); ok {
		return false
	}
	if se, ok := errors.AsType[*StatusError](err); ok {
		return se.Code == http.StatusTooManyRequests || se.Code >= http.StatusInternalServerError
	}
	return true
}

// sendError wraps a failed client.Do: a network error, retried.
func sendError(what string, err error) error {
	return fmt.Errorf("failed to send %s: %w", what, err)
}
