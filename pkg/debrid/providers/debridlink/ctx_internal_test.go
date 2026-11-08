package debridlink

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func newCtxProvider(t *testing.T, host string) *DebridLink {
	t.Helper()
	provider, err := New(
		config.Debrid{Name: "debridlink", APIKey: "token", DownloadAPIKeys: []string{"token"}},
		nil,
		types.ProviderOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	provider.Host = host
	return provider
}

// A listing is whole or an error: when the ctx ends while page 1 is served, the
// pages read so far are not returned as the listing.
func TestGetTorrentsCancelAfterFirstPage(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "0" {
			http.Error(w, "page 1 must not be requested", http.StatusInternalServerError)
			return
		}
		cancel()
		_, _ = fmt.Fprint(w, `{"success":true,"value":[{"id":"a","status":100},{"id":"b","status":100}]}`)
	}))
	defer server.Close()

	torrents, err := newCtxProvider(t, server.URL).GetTorrents(ctx)
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
		_, _ = fmt.Fprint(w, `{"success":true,"value":[]}`)
	}))
	t.Cleanup(server.Close)
	provider := newCtxProvider(t, server.URL)
	link := types.DownloadLink{Link: "https://example.test/f", Token: "token", ID: "dl"}
	operations := map[string]func(ctx context.Context) error{
		"SubmitMagnet": func(ctx context.Context) error {
			_, err := provider.SubmitMagnet(ctx, &types.Torrent{Magnet: &utils.Magnet{Link: "magnet:?xt=urn:btih:abc"}})
			return err
		},
		"CheckStatus": func(ctx context.Context) error {
			_, err := provider.CheckStatus(ctx, &types.Torrent{ID: "id"})
			return err
		},
		"DeleteTorrent": func(ctx context.Context) error { return provider.DeleteTorrent(ctx, "id") },
		"IsAvailable": func(ctx context.Context) error {
			_, err := provider.IsAvailable(ctx, []string{"hash"})
			return err
		},
		"UpdateTorrent": func(ctx context.Context) error { return provider.UpdateTorrent(ctx, &types.Torrent{ID: "id"}) },
		"GetTorrent": func(ctx context.Context) error {
			_, err := provider.GetTorrent(ctx, "id")
			return err
		},
		"GetTorrents": func(ctx context.Context) error {
			_, err := provider.GetTorrents(ctx)
			return err
		},
		"RefreshDownloadLinks": provider.RefreshDownloadLinks,
		"GetProfile": func(ctx context.Context) error {
			_, err := newCtxProvider(t, server.URL).GetProfile(ctx)
			return err
		},
		"GetAvailableSlots": func(ctx context.Context) error {
			_, err := provider.GetAvailableSlots(ctx)
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
