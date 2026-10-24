package qbit

import (
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type deletingProvider struct {
	debrid.Client

	deleted atomic.Value
}

func (*deletingProvider) Config() config.Debrid {
	return config.Debrid{Name: "torbox", Provider: "torbox"}
}
func (*deletingProvider) Logger() zerolog.Logger { return zerolog.Nop() }
func (p *deletingProvider) DeleteTorrent(id string) error {
	p.deleted.Store(id)
	return nil
}

// The arr deleting a torrent still downloading on its provider deletes the provider torrent with the queue entry.
func TestDeleteDownloadingTorrentDeletesProviderTorrent(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, func(cfg *config.Config) { cfg.UseAuth = false })
	provider := &deletingProvider{}
	mgr.Clients().Store("torbox", provider)
	routes := New(mgr, store, zerolog.Nop()).Routes()

	const hash = "0123456789abcdef0123456789abcdef01234567"
	entry := &storage.Entry{
		InfoHash: hash, Name: "release", Category: "sonarr", Protocol: config.ProtocolTorrent,
		ActiveProvider: "torbox", State: storage.EntryStateDownloading, Status: types.TorrentStatusDownloading,
		Providers: map[string]*storage.ProviderEntry{"torbox": {
			Provider: "torbox", ID: "provider-id", Status: types.TorrentStatusDownloading,
		}},
	}
	if err := mgr.Queue().Add(entry); err != nil {
		t.Fatal(err)
	}
	if w := postForm(
		routes,
		"/torrents/delete",
		url.Values{"hashes": {hash}, "deleteFiles": {"false"}},
	); w.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", w.Code, w.Body.String())
	}
	if mgr.Queue().Contains(hash) {
		t.Fatal("queue entry kept")
	}
	if got, _ := provider.deleted.Load().(string); got != "provider-id" {
		t.Fatalf("deleted provider torrent = %q, want provider-id", got)
	}
}
