package premiumize

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/internal/testutil"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// The default wait for a finished transfer's links is short: CheckStatus runs
// inside the qBittorrent add request and inside a queue slot.
func TestDefaultFinalizingWaitIsShort(t *testing.T) {
	t.Parallel()
	if wait := statusPollInterval * maxFinalizingPolls; wait > time.Minute {
		t.Fatalf("a finalizing transfer is polled for %s, want at most a minute", wait)
	}
}

// Shutdown ends the wait between polls at once, with an error that is both a
// pending status (the entry stays queued) and the cancellation.
func TestCheckStatusStopsPollingOnShutdown(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/transfer/list", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"status":"success","transfers":[{"id":"t1","name":"Release","status":"finished",`+
			`"progress":1,"folder_id":"f1","file_id":null}]}`)
	})
	mux.HandleFunc("GET /api/folder/list", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"status":"success","folder_id":"f1","content":[{"id":"x","name":"m.mkv","type":"file",`+
			`"size":1,"link":""}]}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// A poll ends with the folder listing (the file has no link yet, so the transfer is finalizing).
	firstPoll := func(r *http.Request) bool { return r.URL.Path == "/api/folder/list" }
	pm := &Premiumize{
		Host: server.URL,
		client: request.New(zerolog.Nop(), nil, request.WithMaxRetries(0),
			request.WithTransport(testutil.CancelAfterResponse(firstPoll, cancel))),
		config: config.Debrid{Name: "premiumize"},
		logger: zerolog.Nop(),
		options: types.ProviderOptions{
			ValidateFile: func(string, int64) error { return nil },
		},
		pollInterval: time.Hour,
	}

	// The poll interval is an hour, so returning at all means the wait ended on the cancel.
	_, err := pm.CheckStatus(ctx, &types.Torrent{ID: "t1"})

	if !errors.Is(err, customerror.ErrProviderStatusTimeout) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a pending status wrapping the cancellation", err)
	}
	if errors.Is(err, customerror.ErrTorrentNotCached) {
		t.Fatalf("err = %v must not be ErrTorrentNotCached", err)
	}
}
