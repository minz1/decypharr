package realdebrid

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
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// newCtxProvider is a provider with an account, so every Client method has the
// state it needs to reach the network.
func newCtxProvider(host string) *RealDebrid {
	provider := testRealDebrid(host)
	provider.accountsManager = account.NewManager(
		config.Debrid{Name: "realdebrid", DownloadAPIKeys: []string{"token"}},
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
		if r.URL.Query().Get("offset") != "" {
			http.Error(w, "page 2 must not be requested", http.StatusInternalServerError)
			return
		}
		cancel()
		_, _ = fmt.Fprint(w, `[{"id":"a","status":"downloaded"},{"id":"b","status":"downloaded"}]`)
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
		_, _ = fmt.Fprint(w, `{"id":"id","status":"downloaded"}`)
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
			_, err := newCtxProvider(server.URL).GetProfile(ctx)
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

// A ctx that ends while a RAR is being unpacked fails the call. The single
// <name>.rar file is the answer for an archive that cannot be read, not for one
// that was not read.
func TestRealDebridUnpackRarCancelled(t *testing.T) {
	t.Parallel()
	for name, call := range map[string]func(ctx context.Context, p *RealDebrid, tor *types.Torrent) error{
		"CheckStatus": func(ctx context.Context, p *RealDebrid, tor *types.Torrent) error {
			_, err := p.CheckStatus(ctx, tor)
			return err
		},
		"UpdateTorrent": func(ctx context.Context, p *RealDebrid, tor *types.Torrent) error {
			return p.UpdateTorrent(ctx, tor)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var host string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /torrents/info/id":
					_, _ = fmt.Fprint(w, `{"status":"downloaded","filename":"movie","files":[`+
						`{"id":1,"path":"/a.mkv","bytes":10,"selected":1},`+
						`{"id":2,"path":"/b.mkv","bytes":10,"selected":1}],"links":["https://example.test/rar"]}`)
				case "POST /unrestrict/link/":
					_, _ = fmt.Fprintf(w, `{"download":%q,"filename":"movie.rar","filesize":1000}`, host+"/archive")
				case "HEAD /archive":
					w.Header().Set("Content-Length", "1000")
				case "GET /archive":
					cancel()
					<-r.Context().Done()
				default:
					http.Error(w, "unexpected request", http.StatusInternalServerError)
				}
			}))
			defer server.Close()
			host = server.URL
			provider := newCtxProvider(server.URL)
			provider.config.UnpackRar = true
			provider.pollInterval = time.Millisecond
			provider.client = request.New(zerolog.Nop(), nil, request.WithMaxRetries(0))
			provider.rarSemaphore = make(chan struct{}, maxConcurrentRarReads)
			tor := &types.Torrent{ID: "id"}

			err := call(ctx, provider, tor)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s error = %v, want context.Canceled", name, err)
			}
			for file := range tor.Files {
				if file == "movie.rar" {
					t.Fatalf("torrent files = %v, want no fallback movie.rar", tor.Files)
				}
			}
		})
	}
}
