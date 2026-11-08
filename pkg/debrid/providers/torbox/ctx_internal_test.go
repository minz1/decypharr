package torbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func newCtxProvider(host string) *Torbox {
	provider := testTorbox(host)
	provider.accountsManager = account.NewManager(
		config.Debrid{Name: "torbox", DownloadAPIKeys: []string{"token"}},
		types.ProviderOptions{Logger: zerolog.Nop()},
		nil,
	)
	return provider
}

// A listing is whole or an error: when the ctx ends while page 1 is served, the
// pages read so far are not returned as the listing.
func TestGetTorrentsCancelAfterFirstPage(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("offset") != "0" {
			http.Error(w, "page 2 must not be requested", http.StatusInternalServerError)
			return
		}
		cancel()
		_, _ = fmt.Fprint(w, `{"success":true,"data":[{"id":1,"name":"a","download_finished":true},`+
			`{"id":2,"name":"b","download_finished":true}]}`)
	}))
	defer server.Close()

	torrents, err := newCtxProvider(server.URL).GetTorrents(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("GetTorrents() error = %v, want context.Canceled", err)
	}
	if torrents != nil {
		t.Fatalf("GetTorrents() returned %d torrents with the error, want a nil list", len(torrents))
	}
}

// With a cancelled ctx every Client I/O method returns an error wrapping the
// ctx error, never nil and never a fallback.
func TestClientMethodsCancelled(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"success":true,"data":[]}`)
	}))
	t.Cleanup(server.Close)
	provider := newCtxProvider(server.URL)
	link := types.DownloadLink{Link: "torbox://17/1", Token: "token", ID: "dl"}
	operations := map[string]func(ctx context.Context) error{
		"SubmitMagnet": func(ctx context.Context) error {
			_, err := provider.SubmitMagnet(ctx, &types.Torrent{Magnet: &utils.Magnet{Link: "magnet:?xt=urn:btih:abc"}})
			return err
		},
		"CheckStatus": func(ctx context.Context) error {
			_, err := provider.CheckStatus(ctx, &types.Torrent{ID: "17"})
			return err
		},
		"DeleteTorrent": func(ctx context.Context) error { return provider.DeleteTorrent(ctx, "17") },
		"IsAvailable": func(ctx context.Context) error {
			_, err := provider.IsAvailable(ctx, []string{"hash"})
			return err
		},
		"UpdateTorrent": func(ctx context.Context) error { return provider.UpdateTorrent(ctx, &types.Torrent{ID: "17"}) },
		"GetTorrent": func(ctx context.Context) error {
			_, err := provider.GetTorrent(ctx, "17")
			return err
		},
		"GetTorrents": func(ctx context.Context) error {
			_, err := provider.GetTorrents(ctx)
			return err
		},
		"RefreshDownloadLinks": provider.RefreshDownloadLinks,
		"GetProfile": func(ctx context.Context) error {
			_, err := newCtxProvider(server.URL).GetProfile(ctx)
			return err
		},
		"GetAvailableSlots": func(ctx context.Context) error {
			_, err := newCtxProvider(server.URL).GetAvailableSlots(ctx)
			return err
		},
		"syncAccount": func(ctx context.Context) error {
			return provider.syncAccount(ctx, provider.accountsManager.All()[0])
		},
		"DeleteLink": func(ctx context.Context) error { return provider.DeleteLink(ctx, link) },
	}
	for name, op := range operations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := op(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("%s() error = %v, want context.Canceled", name, err)
			}
		})
	}
}
