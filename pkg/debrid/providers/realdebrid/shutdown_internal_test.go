package realdebrid

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
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
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		provider := &RealDebrid{
			// CheckStatus waits before its first poll, so no request is made; a poll
			// after the wait fails to dial and the error check below catches it.
			Host:         "http://127.0.0.1:0",
			client:       request.New(zerolog.Nop(), nil, request.WithMaxRetries(0)),
			logger:       zerolog.Nop(),
			options:      mkvOnly(),
			pollInterval: time.Hour,
		}
		done := make(chan error, 1)
		go func() {
			_, err := provider.CheckStatus(ctx, &types.Torrent{ID: "id"})
			done <- err
		}()

		synctest.Wait()
		cancel()
		err := <-done

		if !errors.Is(err, customerror.ErrProviderStatusTimeout) || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want a pending status wrapping the cancellation", err)
		}
		if errors.Is(err, customerror.ErrTorrentNotCached) {
			t.Fatalf("err = %v must not be ErrTorrentNotCached", err)
		}
	})
}
