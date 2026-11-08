package premiumize

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func newCtxProvider(host string) *Premiumize {
	pm := &Premiumize{
		Host:    host,
		client:  request.New(zerolog.Nop(), nil, request.WithMaxRetries(0)),
		config:  config.Debrid{Name: "premiumize"},
		logger:  zerolog.Nop(),
		options: types.ProviderOptions{ValidateFile: func(string, int64) error { return nil }},
	}
	pm.accountsManager = account.NewManager(
		config.Debrid{Name: "premiumize", DownloadAPIKeys: []string{"token"}},
		types.ProviderOptions{Logger: zerolog.Nop()},
		nil,
	)
	return pm
}

// A transfer whose files cannot be read is skipped, but not when the cause is
// the ctx ending: a listing is whole or an error, never a partial list.
func TestPremiumizeGetTorrentsCancelDuringFileFetch(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/transfer/list", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"status":"success","transfers":[`+
			`{"id":"a","name":"A","status":"finished","progress":1,"folder_id":"fa","file_id":null},`+
			`{"id":"b","name":"B","status":"finished","progress":1,"folder_id":"fb","file_id":null}]}`)
	})
	mux.HandleFunc("GET /api/folder/list", func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		http.Error(w, "gone", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
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
		_, _ = fmt.Fprint(w, `{"status":"success","transfers":[],"content":[]}`)
	}))
	t.Cleanup(server.Close)
	provider := newCtxProvider(server.URL)
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
			_, err := provider.IsAvailable(ctx, []string{"8d2b41ef6a4cd8f42c601c396c1caeebe2aed47d"})
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
