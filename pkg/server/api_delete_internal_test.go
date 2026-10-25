package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// queueCheckingProvider records whether the queue still held the entry when its provider torrent was deleted.
type queueCheckingProvider struct {
	debrid.Client

	queue          *manager.Queue
	hash           string
	queuedAtDelete atomic.Bool
	deleted        chan struct{}
}

func (*queueCheckingProvider) Config() config.Debrid {
	return config.Debrid{Name: "torbox", Provider: "torbox"}
}
func (*queueCheckingProvider) Logger() zerolog.Logger { return zerolog.Nop() }
func (p *queueCheckingProvider) DeleteTorrent(string) error {
	p.queuedAtDelete.Store(p.queue.Contains(p.hash))
	close(p.deleted)
	return nil
}

// A dashboard delete with removeFromDebrid removes the queue record before the provider torrent, so a poll seeing it gone fails nothing.
func TestDashboardDeleteRemovesQueueRecordBeforeProviderTorrent(t *testing.T) {
	t.Parallel()
	const hash = "0123456789abcdef0123456789abcdef01234567"
	for name, tc := range map[string]struct {
		path   string
		status types.TorrentStatus
	}{
		"single":               {"/torrents/sonarr/" + hash + "?removeFromDebrid=true", types.TorrentStatusDownloading},
		"bulk":                 {"/torrents?hashes=" + hash + "&removeFromDebrid=true", types.TorrentStatusDownloading},
		"finished at provider": {"/torrents/sonarr/" + hash + "?removeFromDebrid=true", types.TorrentStatusDownloaded},
		"errored at provider":  {"/torrents/sonarr/" + hash + "?removeFromDebrid=true", types.TorrentStatusError},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := tc.path
			mgr, store := managertest.New(t, func(cfg *config.Config) { cfg.UseAuth = false })
			provider := &queueCheckingProvider{queue: mgr.Queue(), hash: hash, deleted: make(chan struct{})}
			mgr.Clients().Store("torbox", provider)
			if err := mgr.Queue().Add(&storage.Entry{
				InfoHash: hash, Name: "release", Category: "sonarr", Protocol: config.ProtocolTorrent,
				ActiveProvider: "torbox", State: storage.EntryStateDownloading, Status: types.TorrentStatusDownloading,
				Providers: map[string]*storage.ProviderEntry{"torbox": {
					Provider: "torbox", ID: "provider-id", Status: tc.status,
				}},
			}); err != nil {
				t.Fatal(err)
			}
			s := &Server{manager: mgr, config: store, logger: zerolog.Nop()}
			router := chi.NewRouter()
			router.Delete("/torrents/{category}/{hash}", s.handleDeleteTorrent)
			router.Delete("/torrents", s.handleDeleteTorrents)

			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, path, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("delete status = %d: %s", w.Code, w.Body.String())
			}
			select {
			case <-provider.deleted:
			default:
				t.Fatal("the delete returned before it removed the provider torrent")
			}
			if provider.queuedAtDelete.Load() {
				t.Fatal("the provider torrent was deleted while the queue still held the entry")
			}
			if mgr.Queue().Contains(hash) {
				t.Fatal("queue entry kept")
			}
		})
	}
}
