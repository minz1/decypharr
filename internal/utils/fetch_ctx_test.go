package utils_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirrobot01/decypharr/internal/utils"
)

// blockingServer answers no request: its handler reports the request on
// started, then waits for the request's own cancel (reported on sawCancel) or
// for the test to end.
func blockingServer(t *testing.T) (string, <-chan struct{}, <-chan struct{}) {
	t.Helper()
	startedCh := make(chan struct{}, 1)
	sawCancelCh := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedCh <- struct{}{}
		select {
		case <-r.Context().Done():
			sawCancelCh <- struct{}{}
		case <-release:
			http.Error(w, "released", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	return server.URL, startedCh, sawCancelCh
}

// A cancelled caller ends the download: the request carries the caller's
// context instead of a Background one.
func TestDownloadFileHonoursCancel(t *testing.T) {
	t.Parallel()
	url, started, sawCancel := blockingServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, _, err := utils.DownloadFile(ctx, utils.NewDownloadClient(nil), url)
		done <- err
	}()
	<-started
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("DownloadFile err = %v, want context.Canceled", err)
	}
	<-sawCancel
}

// GetMagnetFromURL fetches a .torrent through the same path, so a cancelled
// caller ends it too.
func TestGetMagnetFromURLHonoursCancel(t *testing.T) {
	t.Parallel()
	url, started, sawCancel := blockingServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := utils.GetMagnetFromURL(ctx, utils.NewDownloadClient(nil), url, false)
		done <- err
	}()
	<-started
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("GetMagnetFromURL err = %v, want context.Canceled", err)
	}
	<-sawCancel
}
