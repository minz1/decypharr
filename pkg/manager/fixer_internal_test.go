package manager

import (
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type replacementProvider struct {
	debrid.Client

	submissions int
	deletions   atomic.Int64
}

func (c *replacementProvider) Config() config.Debrid {
	return config.Debrid{Name: "remaining", Provider: "torbox"}
}
func (c *replacementProvider) SubmitMagnet(torrent *types.Torrent) (*types.Torrent, error) {
	c.submissions++
	torrent.ID = "new"
	torrent.Debrid = "remaining"
	torrent.Status = types.TorrentStatusDownloaded
	torrent.Files = map[string]types.File{"video.mkv": {Name: "video.mkv", ID: "file"}}
	return torrent, nil
}
func (c *replacementProvider) CheckStatus(torrent *types.Torrent) (*types.Torrent, error) {
	return torrent, nil
}
func (c *replacementProvider) DeleteTorrent(string) error { c.deletions.Add(1); return nil }

func TestFixTorrentWithRemovedProvider(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "submit replacement", true: "reuse placement"}[existing], func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				checkFixWithRemovedProvider(t, existing)
			})
		})
	}
}

func checkFixWithRemovedProvider(t *testing.T, existing bool) {
	t.Helper()
	store, err := storage.NewStorage(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	client := &replacementProvider{}
	m := withTestConfig(
		t,
		&Manager{storage: store, clients: xsync.NewMap[string, debrid.Client](), logger: zerolog.Nop()},
	)
	m.clients.Store("remaining", client)
	fixer := NewFixer(m)
	fixer.providerOrder = []string{"remaining"}
	entry := &storage.Entry{
		InfoHash: "0123456789012345678901234567890123456789", Name: "release",
		Protocol: config.ProtocolTorrent, ActiveProvider: "removed",
		Providers: map[string]*storage.ProviderEntry{
			"removed": {Provider: "removed", ID: "old", Status: types.TorrentStatusDownloaded},
		},
	}
	if existing {
		entry.Providers["remaining"] = &storage.ProviderEntry{
			Provider: "remaining",
			ID:       "new",
			Status:   types.TorrentStatusDownloaded,
		}
	}
	result, err := fixer.FixTorrent(t.Context(), entry, false)
	if err != nil || !result.Success {
		t.Fatalf("repair = %#v, error = %v", result, err)
	}
	if entry.ActiveProvider != "remaining" {
		t.Fatalf("active provider = %q", entry.ActiveProvider)
	}
	wantSubmissions := 1
	if existing {
		wantSubmissions = 0
	}
	if client.submissions != wantSubmissions {
		t.Fatalf("submissions = %d, want %d", client.submissions, wantSubmissions)
	}
	synctest.Wait()
	if client.deletions.Load() != 0 {
		t.Fatal("replacement provider received a deletion")
	}
	loaded, err := store.Get(entry.InfoHash)
	if err != nil || loaded.ActiveProvider != "remaining" {
		t.Fatalf("saved entry = %#v, error = %v", loaded, err)
	}
	if loaded.Providers["removed"].ID != "old" {
		t.Fatal("source placement changed")
	}
}

// Every caller that joins an in-flight repair must receive its result; a
// single-slot result channel used to release only one of them.
func TestFixTorrentReleasesAllWaiters(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fixer := NewFixer(withTestConfig(t, &Manager{logger: zerolog.Nop()}))
		entry := &storage.Entry{InfoHash: "hash", Name: "release", Protocol: config.ProtocolTorrent}
		inFlight := &FixerRequest{InfoHash: entry.InfoHash, done: make(chan struct{})}
		fixer.inFlightRepairs.Store(entry.InfoHash, inFlight)

		const waiters = 3
		results := make(chan *FixResult, waiters)
		for range waiters {
			go func() {
				result, err := fixer.FixTorrent(t.Context(), entry, false)
				if err != nil {
					t.Errorf("waiter error: %v", err)
				}
				results <- result
			}()
		}
		synctest.Wait()
		inFlight.result = &FixResult{Success: true, NewDebrid: "remaining"}
		close(inFlight.done)
		for range waiters {
			if result := <-results; result == nil || !result.Success {
				t.Fatalf("waiter result = %#v", result)
			}
		}
	})
}

func TestResetFailureStateClearsPerDebridFailures(t *testing.T) {
	t.Parallel()
	fixer := NewFixer(withTestConfig(t, &Manager{logger: zerolog.Nop()}))
	fixer.failedToReinsert.Store(failureKey("hash", "provider"), struct{}{})
	fixer.failedToReinsert.Store("hash", struct{}{})
	fixer.failedToReinsert.Store(failureKey("other", "provider"), struct{}{})

	fixer.ResetFailureState("hash")

	if fixer.IsFailedToReinsert("hash", "provider") {
		t.Fatal("per-debrid failure survived a reset")
	}
	if !fixer.IsFailedToReinsert("other", "provider") {
		t.Fatal("reset cleared another torrent's failure")
	}
}
