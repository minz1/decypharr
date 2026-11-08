package alldebrid

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

// With a cancelled ctx every Client I/O method returns an error wrapping the
// ctx error, never nil and never a fallback.
func TestClientMethodsCancelled(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"status":"success","data":{}}`)
	}))
	t.Cleanup(server.Close)
	provider := testAllDebrid(server.URL)
	provider.accountsManager = account.NewManager(
		config.Debrid{Name: "alldebrid", DownloadAPIKeys: []string{"token"}},
		types.ProviderOptions{Logger: zerolog.Nop()},
		nil,
	)
	link := types.DownloadLink{Link: "https://example.test/f", Token: "token", ID: "dl"}
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
			_, err := testAllDebrid(server.URL).GetProfile(ctx)
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
