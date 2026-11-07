package realdebrid

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// The default wait for a converting magnet is short: CheckStatus runs inside
// the qBittorrent add request and inside a queue slot.
func TestDefaultConversionWaitIsShort(t *testing.T) {
	t.Parallel()
	if wait := statusPollInterval * maxConversionPolls; wait > time.Minute {
		t.Fatalf("a converting magnet is polled for %s, want at most a minute", wait)
	}
}

// Shutdown ends the wait between polls at once, with an error that is both a
// pending status (the entry stays queued) and the cancellation.
func TestCheckStatusStopsPollingOnShutdown(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"magnet_conversion","filename":"movie"}`))
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	options := mkvOnly()
	options.Shutdown = func() context.Context { return ctx }
	provider := &RealDebrid{
		Host:         server.URL,
		client:       request.New(zerolog.Nop(), nil, request.WithMaxRetries(0)),
		logger:       zerolog.Nop(),
		options:      options,
		pollInterval: time.Hour,
	}
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	_, err := provider.CheckStatus(&types.Torrent{ID: "id"})

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("CheckStatus took %s to notice the shutdown", elapsed)
	}
	if !errors.Is(err, customerror.ErrProviderStatusTimeout) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a pending status wrapping the cancellation", err)
	}
	if errors.Is(err, customerror.ErrTorrentNotCached) {
		t.Fatalf("err = %v must not be ErrTorrentNotCached", err)
	}
}
