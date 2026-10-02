// Package commontest holds test helpers shared by the debrid providers.
package commontest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// cancelTimeout bounds how long AssertCancellation waits for each step.
const cancelTimeout = 3 * time.Second

// AssertCancellation starts a server that holds every request open until it
// is cancelled, runs op against it, cancels op's context before or during the
// request, and fails t unless op returns context.Canceled promptly.
func AssertCancellation(t *testing.T, cancelBefore bool, op func(ctx context.Context, host string) error) {
	t.Helper()
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if cancelBefore {
		cancel()
	}
	done := make(chan error, 1)
	go func() { done <- op(ctx, server.URL) }()
	if !cancelBefore {
		select {
		case <-started:
		case <-time.After(cancelTimeout):
			t.Fatal("request did not start")
		}
		cancel()
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("operation = %v, want context.Canceled", err)
		}
	case <-time.After(cancelTimeout):
		t.Fatal("operation did not stop after cancellation")
	}
}

// CancelCases names the two cancellation points AssertCancellation covers.
func CancelCases() map[string]bool {
	return map[string]bool{"before request": true, "during request": false}
}
