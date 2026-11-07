package manager

import (
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet"
)

const claimTestHash = "0123456789012345678901234567890123456789"

// newClaimTestManager returns a manager, stopped and with its storage closed
// when the test ends, and a count of the post-download actions it starts.
func newClaimTestManager(t *testing.T) (*Manager, *atomic.Int32) {
	t.Helper()
	m := newShutdownTestManager(t, t.TempDir())
	m.mountManager = shutdownMount{stop: func() error { return nil }}
	var starts atomic.Int32
	m.logger = zerolog.New(io.Discard).Hook(zerolog.HookFunc(func(_ *zerolog.Event, _ zerolog.Level, msg string) {
		if msg == "Download completed, processing action" {
			starts.Add(1)
		}
	}))
	m.downloader = NewDownloadManager(m)
	return m, &starts
}

// endActions cancels the in-flight actions and waits for them to return.
func endActions(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.work.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// newClaimTestEntry queues a finished single-release torrent whose files never
// reach the mount, so its symlink action stays in flight until shutdown.
func newClaimTestEntry(t *testing.T, m *Manager) *storage.Entry {
	t.Helper()
	entry := &storage.Entry{
		InfoHash:        claimTestHash,
		Name:            "Show.S01E01",
		Protocol:        config.ProtocolTorrent,
		State:           storage.EntryStateDownloading,
		ActiveProvider:  "provider",
		Action:          config.DownloadActionSymlink,
		SkipMultiSeason: true,
		SavePath:        t.TempDir(),
		Files:           map[string]*storage.File{},
		Providers:       map[string]*storage.ProviderEntry{},
	}
	applyDebridTorrentToEntry(entry, finishedClaimTorrent())
	if err := m.queue.Add(entry); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.GetTorrentMountPath(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	return entry
}

func finishedClaimTorrent() *types.Torrent {
	return &types.Torrent{
		ID: "provider-id", Debrid: "provider", InfoHash: claimTestHash, Name: "Show.S01E01",
		Status: types.TorrentStatusDownloaded, Progress: 100,
		Files: map[string]types.File{"Show.S01E01.mkv": {Name: "Show.S01E01.mkv", ID: "1", Size: 5}},
	}
}

// Two starts for one entry (the job's hand-off racing another) must drive one
// post-download action: a second would open the same destination files again.
func TestConcurrentActionStartsRunTheActionOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, starts := newClaimTestManager(t)
		newClaimTestEntry(t, m)

		var wg sync.WaitGroup
		for range 8 {
			// Each caller holds its own copy, as separate workers would.
			entry, err := m.queue.GetTorrent(claimTestHash)
			if err != nil {
				t.Fatal(err)
			}
			wg.Go(func() { m.processNewTorrent(entry, finishedClaimTorrent()) })
		}
		wg.Wait()
		synctest.Wait()

		if got := starts.Load(); got != 1 {
			t.Fatalf("post-download actions started = %d, want 1", got)
		}
	})
}

// The poller skips an entry whose hash is claimed. An action started by the
// job hand-off must hold that claim for as long as it runs, and give it back.
func TestActionStartHoldsTheClaimUntilTheActionReturns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, starts := newClaimTestManager(t)
		entry := newClaimTestEntry(t, m)

		m.processNewTorrent(entry, finishedClaimTorrent())
		synctest.Wait()
		if starts.Load() != 1 {
			t.Fatalf("post-download actions started = %d, want 1", starts.Load())
		}
		if _, claimed := m.processingEntries.Load(claimTestHash); !claimed {
			t.Fatal("the running action does not hold the entry's claim")
		}

		endActions(t, m)
		if _, claimed := m.processingEntries.Load(claimTestHash); claimed {
			t.Fatal("the claim outlived its action")
		}
	})
}

// An entry a local download already drives, but whose claim is not held, is not
// started again from a stale copy.
func TestActionStartSkipsAnEntryAlreadyDownloading(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, starts := newClaimTestManager(t)
		entry := newClaimTestEntry(t, m)
		driven, err := m.queue.GetTorrent(claimTestHash)
		if err != nil {
			t.Fatal(err)
		}
		driven.IsDownloading = true
		if updateErr := m.queue.Update(driven); updateErr != nil {
			t.Fatal(updateErr)
		}

		m.startProcessAction(entry)
		synctest.Wait()

		if starts.Load() != 0 {
			t.Fatalf("post-download actions started = %d, want 0 for an entry already downloading", starts.Load())
		}
		if _, claimed := m.processingEntries.Load(claimTestHash); claimed {
			t.Fatal("a skipped start leaked the claim")
		}
	})
}

// A hand-off that arrives after the entry was deleted, or already finished, must
// not run the action again: it would rewrite the destination files.
func TestActionStartSkipsAnEntryThatIsGoneOrFinished(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, starts := newClaimTestManager(t)
		entry := newClaimTestEntry(t, m)

		finished, err := m.queue.GetTorrent(claimTestHash)
		if err != nil {
			t.Fatal(err)
		}
		finished.MarkAsCompleted(finished.DownloadPath(""))
		if updateErr := m.queue.Update(finished); updateErr != nil {
			t.Fatal(updateErr)
		}
		m.startProcessAction(entry)
		synctest.Wait()
		if starts.Load() != 0 {
			t.Fatalf("an already finished entry started %d actions", starts.Load())
		}

		if deleteErr := m.queue.Delete(claimTestHash, false, nil); deleteErr != nil {
			t.Fatal(deleteErr)
		}
		m.startProcessAction(entry)
		synctest.Wait()
		if starts.Load() != 0 {
			t.Fatalf("a deleted entry started %d actions", starts.Load())
		}
		if _, claimed := m.processingEntries.Load(claimTestHash); claimed {
			t.Fatal("a skipped start leaked the claim")
		}
	})
}

// The poll runs the action inline under its own claim: it runs exactly once, and
// a job hand-off arriving meanwhile finds the claim held.
func TestQueuedTorrentPollRunsTheActionOnceUnderItsClaim(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, starts := newClaimTestManager(t)
		entry := newClaimTestEntry(t, m)
		m.clients.Store("provider", completedTorrentProvider{torrent: finishedClaimTorrent()})

		m.processQueuedEntries()
		synctest.Wait()
		if got := starts.Load(); got != 1 {
			t.Fatalf("post-download actions started = %d, want 1", got)
		}
		if _, claimed := m.processingEntries.Load(claimTestHash); !claimed {
			t.Fatal("the poll gave up its claim while the action runs")
		}

		m.processNewTorrent(entry, finishedClaimTorrent())
		m.processQueuedEntries()
		synctest.Wait()
		if got := starts.Load(); got != 1 {
			t.Fatalf("a hand-off or poll started a second action: starts = %d", got)
		}
		endActions(t, m)
		if _, claimed := m.processingEntries.Load(claimTestHash); claimed {
			t.Fatal("the claim outlived the poll")
		}
	})
}

// A poll holds a listing taken before a hand-off action finished the entry. The
// stale copy must not drive it: an NZB poll would write the deleted entry back
// and run the action again.
func TestPollOfAListedEntryDeletedMeanwhileWritesNothingBack(t *testing.T) {
	t.Parallel()
	usenetClient := newUnreachableUsenet(t)
	synctest.Test(t, func(t *testing.T) {
		m, starts := newClaimTestManager(t)
		m.usenet = usenetClient
		listed, meta := newClaimTestNZB(t, m)
		if err := m.usenet.NZBStorage().AddNZB(meta); err != nil {
			t.Fatal(err)
		}
		snapshot, err := m.queue.GetTorrent(claimTestHash)
		if err != nil {
			t.Fatal(err)
		}
		if deleteErr := m.queue.Delete(claimTestHash, false, nil); deleteErr != nil {
			t.Fatal(deleteErr)
		}

		m.pollEntry(snapshot)
		synctest.Wait()

		if starts.Load() != 0 {
			t.Fatalf("a deleted entry started %d actions", starts.Load())
		}
		if m.queue.Contains(claimTestHash) {
			t.Fatalf("the poll wrote the deleted entry %q back", listed.Name)
		}
		if _, claimed := m.processingEntries.Load(claimTestHash); claimed {
			t.Fatal("a skipped poll leaked the claim")
		}
	})
}

// The same for an entry a hand-off has since marked as failed: the poll must
// not run it again.
func TestPollOfAListedEntryErroredMeanwhileDoesNotRunIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, starts := newClaimTestManager(t)
		newClaimTestEntry(t, m)
		m.clients.Store("provider", completedTorrentProvider{torrent: finishedClaimTorrent()})
		snapshot, err := m.queue.GetTorrent(claimTestHash)
		if err != nil {
			t.Fatal(err)
		}
		failed, err := m.queue.GetTorrent(claimTestHash)
		if err != nil {
			t.Fatal(err)
		}
		failed.MarkAsError(errEntryDeleted)
		if updateErr := m.queue.Update(failed); updateErr != nil {
			t.Fatal(updateErr)
		}

		m.pollEntry(snapshot)
		synctest.Wait()

		if starts.Load() != 0 {
			t.Fatalf("an errored entry started %d actions", starts.Load())
		}
		if _, claimed := m.processingEntries.Load(claimTestHash); claimed {
			t.Fatal("a skipped poll leaked the claim")
		}
	})
}

// A hand-off skipped because a poll held the claim (the poll saw the NZB still
// parsing) must not leave the action unrun: the next poll runs it, once.
func TestHandOffSkippedBehindAPollIsRunByTheNextPoll(t *testing.T) {
	t.Parallel()
	usenetClient := newUnreachableUsenet(t)
	synctest.Test(t, func(t *testing.T) {
		m, starts := newClaimTestManager(t)
		m.usenet = usenetClient
		entry, meta := newClaimTestNZB(t, m)
		meta.Status = usenet.NZBStatusParsing
		if err := m.usenet.NZBStorage().AddNZB(meta); err != nil {
			t.Fatal(err)
		}

		// The poll holds the claim while it reads the parsing meta.
		m.processingEntries.Store(claimTestHash, struct{}{})
		if err := m.processNZB(entry, meta); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if starts.Load() != 0 {
			t.Fatalf("a hand-off ran behind the poll's claim: starts = %d", starts.Load())
		}
		m.processingEntries.Delete(claimTestHash)

		meta.Status = usenet.NZBStatusCompleted
		if err := m.usenet.NZBStorage().AddNZB(meta); err != nil {
			t.Fatal(err)
		}
		m.processQueuedEntries()
		synctest.Wait()
		m.processQueuedEntries()
		synctest.Wait()

		if got := starts.Load(); got != 1 {
			t.Fatalf("post-download actions started = %d, want exactly 1", got)
		}
	})
}

// newClaimTestNZB queues a processed NZB whose files never reach the mount.
func newClaimTestNZB(t *testing.T, m *Manager) (*storage.Entry, *storage.NZB) {
	t.Helper()
	meta := &storage.NZB{
		ID: claimTestHash, Name: "Show.S01E01", Status: usenet.NZBStatusCompleted, TotalSize: 5,
		Files: []storage.NZBFile{{Name: "Show.S01E01.mkv", Size: 5}},
	}
	entry := &storage.Entry{
		InfoHash:       claimTestHash,
		Name:           "Show.S01E01",
		Protocol:       config.ProtocolNZB,
		State:          storage.EntryStateDownloading,
		Status:         types.TorrentStatusDownloading,
		ActiveProvider: usenetProvider,
		Action:         config.DownloadActionSymlink,
		SavePath:       t.TempDir(),
		Files:          map[string]*storage.File{},
		Providers:      map[string]*storage.ProviderEntry{},
	}
	_ = entry.AddUsenetProvider(meta)
	if err := m.queue.Add(entry); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.GetTorrentMountPath(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	return entry, meta
}

// The job's hand-off of a processed NZB takes the same claim.
func TestConcurrentNZBStartsRunTheActionOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, starts := newClaimTestManager(t)
		_, meta := newClaimTestNZB(t, m)

		var wg sync.WaitGroup
		for range 8 {
			entry, err := m.queue.GetTorrent(claimTestHash)
			if err != nil {
				t.Fatal(err)
			}
			wg.Go(func() {
				if processErr := m.processNZB(entry, meta); processErr != nil {
					t.Error(processErr)
				}
			})
		}
		wg.Wait()
		synctest.Wait()

		if got := starts.Load(); got != 1 {
			t.Fatalf("post-download actions started = %d, want 1", got)
		}
		if _, claimed := m.processingEntries.Load(claimTestHash); !claimed {
			t.Fatal("the running action does not hold the entry's claim")
		}
	})
}

// A poll already holds the entry's claim, so it runs the action itself rather
// than skipping it behind its own claim.
func TestQueuedNZBPollRunsTheActionUnderItsClaim(t *testing.T) {
	t.Parallel()
	// The client's connection goroutines must not live in the test's bubble.
	usenetClient := newUnreachableUsenet(t)
	synctest.Test(t, func(t *testing.T) {
		m, starts := newClaimTestManager(t)
		m.usenet = usenetClient
		entry, meta := newClaimTestNZB(t, m)
		if err := m.usenet.NZBStorage().AddNZB(meta); err != nil {
			t.Fatal(err)
		}

		m.processingEntries.Store(claimTestHash, struct{}{})
		_ = m.runWork("poll", func() { m.processQueuedNZB(entry) })
		synctest.Wait()

		if got := starts.Load(); got != 1 {
			t.Fatalf("post-download actions started = %d, want 1", got)
		}
		if _, claimed := m.processingEntries.Load(claimTestHash); !claimed {
			t.Fatal("the poll gave up its claim while the action runs")
		}
		endActions(t, m)
		if _, claimed := m.processingEntries.Load(claimTestHash); claimed {
			t.Fatal("the claim outlived the poll")
		}
	})
}
