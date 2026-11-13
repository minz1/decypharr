package qbit

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type deletingProvider struct {
	debrid.Client

	deleted chan string
}

func (*deletingProvider) Config() config.Debrid {
	return config.Debrid{Name: "torbox", Provider: "torbox"}
}
func (*deletingProvider) Logger() zerolog.Logger { return zerolog.Nop() }
func (p *deletingProvider) DeleteTorrent(_ context.Context, id string) error {
	p.deleted <- id
	return nil
}

// The arr deleting a torrent still downloading on its provider deletes the provider torrent with the queue entry.
func TestDeleteDownloadingTorrentDeletesProviderTorrent(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, func(cfg *config.Config) { cfg.UseAuth = false })
	provider := &deletingProvider{deleted: make(chan string, 1)}
	mgr.Clients().Store("torbox", provider)
	routes := New(mgr, store, zerolog.Nop()).Routes()

	const hash = "0123456789abcdef0123456789abcdef01234567"
	entry := &storage.Entry{
		InfoHash: hash, Name: "release", Category: "sonarr", Protocol: config.ProtocolTorrent,
		ActiveProvider: "torbox", State: storage.EntryStateDownloading, Status: storage.EntryStatusDownloading,
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
	if err := mgr.AwaitGone(t.Context(), hash); err != nil {
		t.Fatal(err)
	}
	if mgr.Queue().Contains(hash) {
		t.Fatal("queue entry kept")
	}
	select {
	case got := <-provider.deleted:
		if got != "provider-id" {
			t.Fatalf("deleted provider torrent = %q, want provider-id", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the provider torrent was never deleted")
	}
}

// The arr deleting a failed grab whose placement still holds a slot deletes the provider torrent with it.
func TestDeleteFailedTorrentWithFilesDeletesProviderTorrent(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, func(cfg *config.Config) { cfg.UseAuth = false })
	provider := &deletingProvider{deleted: make(chan string, 1)}
	mgr.Clients().Store("torbox", provider)
	routes := New(mgr, store, zerolog.Nop()).Routes()

	const hash = "0123456789abcdef0123456789abcdef01234568"
	failedAt := time.Now().Add(-time.Hour)
	entry := &storage.Entry{
		InfoHash: hash, Name: "release", Category: "sonarr", Protocol: config.ProtocolTorrent,
		ActiveProvider: "torbox", State: storage.EntryStateError, Status: storage.EntryStatusError,
		LastError: "no seeders", LastErrorTime: &failedAt, ErrorCount: 1,
		Providers: map[string]*storage.ProviderEntry{"torbox": {
			Provider: "torbox", ID: "stuck-id", Status: types.TorrentStatusDownloading,
		}},
	}
	if err := mgr.Queue().Add(entry); err != nil {
		t.Fatal(err)
	}
	if w := postForm(
		routes,
		"/torrents/delete",
		url.Values{"hashes": {hash}, "deleteFiles": {"true"}},
	); w.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", w.Code, w.Body.String())
	}
	if err := mgr.AwaitGone(t.Context(), hash); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-provider.deleted:
		if got != "stuck-id" {
			t.Fatalf("deleted provider torrent = %q, want stuck-id", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the provider torrent was never deleted")
	}
}
