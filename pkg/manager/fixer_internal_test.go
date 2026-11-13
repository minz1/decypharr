package manager

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
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
func (c *replacementProvider) SubmitMagnet(_ context.Context, torrent *types.Torrent) (*types.Torrent, error) {
	c.submissions++
	torrent.ID = "new"
	torrent.Debrid = "remaining"
	torrent.Status = types.TorrentStatusDownloaded
	torrent.Files = map[string]types.File{"video.mkv": {Name: "video.mkv", ID: "file"}}
	return torrent, nil
}
func (c *replacementProvider) CheckStatus(_ context.Context, torrent *types.Torrent) (*types.Torrent, error) {
	return torrent, nil
}
func (c *replacementProvider) DeleteTorrent(context.Context, string) error {
	c.deletions.Add(1)
	return nil
}

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
		&Manager{
			work:    testGroup(t),
			storage: store,
			clients: xsync.NewMap[string, debrid.Client](),
			logger:  zerolog.Nop(),
		},
	)
	m.clients.Store("remaining", client)
	fixer := NewFixer(m)
	fixer.providerOrder = []string{"remaining"}
	stamp := time.Now()
	entry := &storage.Entry{
		InfoHash: "0123456789012345678901234567890123456789", Name: "release",
		Protocol: config.ProtocolTorrent, ActiveProvider: "removed", IsComplete: true,
		Providers: map[string]*storage.ProviderEntry{
			"removed": {Provider: "removed", ID: "old", Status: types.TorrentStatusDownloaded, DownloadedAt: &stamp},
		},
	}
	if existing {
		entry.Providers["remaining"] = &storage.ProviderEntry{
			Provider:     "remaining",
			ID:           "new",
			Status:       types.TorrentStatusDownloaded,
			DownloadedAt: &stamp,
		}
	}
	result, err := fixer.FixTorrent(t.Context(), entry, false)
	if err != nil || !result.Success {
		t.Fatalf("repair = %#v, error = %v", result, err)
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
	if loaded.Providers["remaining"].DownloadedAt == nil {
		t.Fatal("adopted placement has no DownloadedAt")
	}
	if loaded.Providers["removed"].ID != "old" {
		t.Fatal("source placement changed")
	}
}

// refusingProvider refuses every submit with its own error.
type refusingProvider struct {
	replacementProvider

	err error
}

func (c *refusingProvider) SubmitMagnet(context.Context, *types.Torrent) (*types.Torrent, error) {
	return nil, c.err
}

func TestRunRepairLogsEveryAttempt(t *testing.T) {
	t.Parallel()
	store, err := storage.NewStorage(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var logs bytes.Buffer
	m := withTestConfig(t, &Manager{
		storage: store, clients: xsync.NewMap[string, debrid.Client](), logger: zerolog.New(&logs),
		entry:         newEntryCache(func(string) bool { return false }, nil, nil),
		confirmBroken: func(context.Context, *storage.Entry) bool { return false },
	})
	m.clients.Store("first", &refusingProvider{err: errors.New("first refused")})
	m.clients.Store("second", &refusingProvider{err: errors.New("second refused")})
	fixer := NewFixer(m)
	fixer.providerOrder = []string{"first", "second"}
	entry := &storage.Entry{
		InfoHash: "0123456789012345678901234567890123456789", Name: "release",
		Protocol: config.ProtocolTorrent, ActiveProvider: "removed",
		Providers: map[string]*storage.ProviderEntry{
			"removed": {Provider: "removed", ID: "old", Status: types.TorrentStatusDownloaded},
		},
	}
	if result, fixErr := fixer.FixTorrent(t.Context(), entry, false); fixErr == nil || result.Success {
		t.Fatalf("repair = %#v, error = %v; want a failure", result, fixErr)
	}
	for _, want := range []string{"first", "first refused", "second", "second refused"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log = %s; want it to name %q", logs.String(), want)
		}
	}
	if got := strings.Count(logs.String(), "re-insert attempt failed"); got != 2 {
		t.Fatalf("attempt lines = %d, want 2", got)
	}
}

// A re-insert that clears a bad mark shows the entry again; one on an unmarked entry refreshes nothing.
func TestMoveTorrentRefreshesWhenClearingBad(t *testing.T) {
	t.Parallel()
	for _, bad := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			store, err := storage.NewStorage(t.TempDir(), storage.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			mount := &countingMount{}
			m := withTestConfig(t, &Manager{
				storage: store, clients: xsync.NewMap[string, debrid.Client](), logger: zerolog.Nop(),
				mountManager: mount, entry: newEntryCache(func(string) bool { return false }, nil, nil),
			})
			m.clients.Store("remaining", &replacementProvider{})
			fixer := NewFixer(m)
			fixer.providerOrder = []string{"remaining"}
			entry := &storage.Entry{
				InfoHash: "0123456789012345678901234567890123456789", Name: "release",
				Protocol: config.ProtocolTorrent, ActiveProvider: "removed", Bad: bad,
				Providers: map[string]*storage.ProviderEntry{
					"removed":   {Provider: "removed", ID: "old", Status: types.TorrentStatusDownloaded},
					"remaining": {Provider: "remaining", ID: "new", Status: types.TorrentStatusDownloaded},
				},
			}
			if result, fixErr := fixer.FixTorrent(t.Context(), entry, false); fixErr != nil || !result.Success {
				t.Fatalf("repair = %#v, error = %v", result, fixErr)
			}
			synctest.Wait()
			loaded, err := store.Get(entry.InfoHash)
			if err != nil || loaded.Bad {
				t.Fatalf("saved entry = %+v, %v; want not bad", loaded, err)
			}
			if want := map[bool]int{true: 1, false: 0}[bad]; mount.count() != want {
				t.Fatalf("bad = %v: mount refreshes = %d, want %d", bad, mount.count(), want)
			}
		})
	}
}

// Every caller that joins an in-flight repair must receive its result; a
// single-slot result channel used to release only one of them.
func TestFixTorrentReleasesAllWaiters(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fixer := NewFixer(withTestConfig(t, &Manager{work: testGroup(t), logger: zerolog.Nop()}))
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
	fixer := NewFixer(withTestConfig(t, &Manager{work: testGroup(t), logger: zerolog.Nop()}))
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

// healProvider lets a test script the provider calls of a heal. A nil hook falls back to replacementProvider.
type healProvider struct {
	replacementProvider

	submit func(ctx context.Context, t *types.Torrent) (*types.Torrent, error)
	check  func(ctx context.Context, t *types.Torrent) (*types.Torrent, error)
}

func (c *healProvider) SubmitMagnet(ctx context.Context, t *types.Torrent) (*types.Torrent, error) {
	if c.submit != nil {
		return c.submit(ctx, t)
	}
	return c.replacementProvider.SubmitMagnet(ctx, t)
}

func (c *healProvider) CheckStatus(ctx context.Context, t *types.Torrent) (*types.Torrent, error) {
	if c.check != nil {
		return c.check(ctx, t)
	}
	return c.replacementProvider.CheckStatus(ctx, t)
}

// healHarness is a fixer over one provider whose work group a test can cancel.
type healHarness struct {
	fixer      *Fixer
	entry      *storage.Entry
	cancelWork context.CancelFunc
	confirms   atomic.Int64 // give-up checks: giveUpOnEntry asks confirmBroken before it marks anything
}

func newHealHarness(t *testing.T, client debrid.Client) *healHarness {
	t.Helper()
	store, err := storage.NewStorage(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	parent, cancelWork := context.WithCancel(context.WithoutCancel(t.Context()))
	work := task.NewRoot(parent, "test", zerolog.Nop())
	t.Cleanup(func() {
		ctx, cancel := task.Detached(t.Context(), testStopTimeout)
		defer cancel()
		if stopErr := work.Stop(ctx); stopErr != nil {
			t.Error(stopErr)
		}
		cancelWork()
	})
	h := &healHarness{cancelWork: cancelWork}
	m := withTestConfig(t, &Manager{
		work: work, storage: store, clients: xsync.NewMap[string, debrid.Client](), logger: zerolog.Nop(),
		entry: newEntryCache(func(string) bool { return false }, nil, nil),
		confirmBroken: func(context.Context, *storage.Entry) bool {
			h.confirms.Add(1)
			return false
		},
	})
	m.life = newLifecycle()
	m.initGrace(t.Context())
	m.clients.Store("remaining", client)
	h.fixer = NewFixer(m)
	h.fixer.providerOrder = []string{"remaining"}
	h.entry = &storage.Entry{
		InfoHash: "0123456789012345678901234567890123456789", Name: "release",
		Protocol: config.ProtocolTorrent, ActiveProvider: "removed",
		Providers: map[string]*storage.ProviderEntry{
			"removed": {Provider: "removed", ID: "old", Status: types.TorrentStatusDownloaded},
		},
	}
	return h
}

type fixOutcome struct {
	result *FixResult
	err    error
}

// fixAsync runs the heal on ctx and returns its outcome channel.
func (h *healHarness) fixAsync(ctx context.Context) <-chan fixOutcome {
	out := make(chan fixOutcome, 1)
	go func() {
		result, err := h.fixer.FixTorrent(ctx, h.entry, false)
		out <- fixOutcome{result, err}
	}()
	return out
}

// A stop during a provider call is not a failed heal: no give-up (which fails the grab in the Arr), no memo, no
// delete of a torrent that was submitted.
func TestFixStopMidHealNoGiveUp(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"submit", "check"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			entered := make(chan struct{})
			block := func(ctx context.Context, torrent *types.Torrent) (*types.Torrent, error) {
				close(entered)
				<-ctx.Done()
				return torrent, ctx.Err()
			}
			client := &healProvider{}
			if stage == "submit" {
				client.submit = func(ctx context.Context, torrent *types.Torrent) (*types.Torrent, error) {
					_, err := block(ctx, torrent)
					return nil, err
				}
			} else {
				client.check = block
			}
			h := newHealHarness(t, client)
			// The reader's ctx ends with the stop too, as a player's does when the process goes down.
			handed, cancel := context.WithCancel(h.fixer.manager.work.Context())
			defer cancel()
			out := h.fixAsync(handed)
			<-entered
			h.cancelWork()
			got := <-out
			if !errors.Is(got.err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", got.err)
			}
			if n := h.confirms.Load(); n != 0 {
				t.Fatalf("give-up checks = %d, want 0", n)
			}
			if n := h.fixer.failedToReinsert.Size(); n != 0 {
				t.Fatalf("failure memos = %d, want 0", n)
			}
			if n := client.deletions.Load(); n != 0 {
				t.Fatalf("deletes = %d, want 0", n)
			}
		})
	}
}

// A config save cancels the repair run's ctx, not the work ctx: the provider call goes on and the heal completes.
func TestFixRunCtxCancelHealCompletes(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	client := &healProvider{}
	client.submit = func(ctx context.Context, torrent *types.Torrent) (*types.Torrent, error) {
		close(entered)
		<-release
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return client.replacementProvider.SubmitMagnet(ctx, torrent)
	}
	client.check = func(ctx context.Context, torrent *types.Torrent) (*types.Torrent, error) {
		if err := ctx.Err(); err != nil {
			return torrent, err
		}
		return torrent, nil
	}
	h := newHealHarness(t, client)
	run, cancelRun := context.WithCancel(t.Context())
	defer cancelRun()
	out := h.fixAsync(run)
	<-entered
	cancelRun()
	close(release)
	got := <-out
	if got.err != nil || got.result == nil || !got.result.Success {
		t.Fatalf("heal = %#v, error = %v; want success", got.result, got.err)
	}
	stored, getErr := h.fixer.manager.storage.Get(h.entry.InfoHash)
	if getErr != nil || stored.ActiveProvider != "remaining" {
		t.Fatalf("stored entry = %+v, %v; want remaining active", stored, getErr)
	}
	if n := h.fixer.failedToReinsert.Size(); n != 0 {
		t.Fatalf("failure memos = %d, want 0", n)
	}
}

// Only the work ctx makes a failure a stop: a provider that failed for real on a live work ctx is memoised even
// when the reader that asked for the heal has gone.
func TestFixReaderCancelAfterFailureStoresMemo(t *testing.T) {
	t.Parallel()
	var cancelReader context.CancelFunc
	client := &healProvider{}
	client.submit = func(context.Context, *types.Torrent) (*types.Torrent, error) {
		cancelReader()
		return nil, errors.New("provider refused")
	}
	h := newHealHarness(t, client)
	var reader context.Context
	reader, cancelReader = context.WithCancel(t.Context())
	defer cancelReader()
	got := <-h.fixAsync(reader)
	if got.err == nil || got.result == nil || got.result.Success {
		t.Fatalf("heal = %#v, error = %v; want a failure", got.result, got.err)
	}
	if !h.fixer.IsFailedToReinsert(h.entry.InfoHash, "remaining") {
		t.Fatal("per-debrid memo was not stored")
	}
	if _, ok := h.fixer.failedToReinsert.Load(h.entry.InfoHash); !ok {
		t.Fatal("whole-torrent memo was not stored")
	}
	if n := h.confirms.Load(); n != 1 {
		t.Fatalf("give-up checks = %d, want 1", n)
	}
}

// The caller's entry is read by its other users while a repair runs; the repair works on its own copy.
func TestFixTorrentLeavesTheCallersEntryAlone(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	client := &healProvider{}
	client.submit = func(ctx context.Context, torrent *types.Torrent) (*types.Torrent, error) {
		close(entered)
		<-release
		return client.replacementProvider.SubmitMagnet(ctx, torrent)
	}
	h := newHealHarness(t, client)
	h.entry.Bad = true
	h.entry.Files = map[string]*storage.File{"video.mkv": {Name: "video.mkv", Deleted: true}}
	before := h.entry.Clone()

	stop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = len(h.entry.Providers) + len(h.entry.Files) + len(h.entry.ActiveProvider)
			_ = h.entry.Files["video.mkv"].Deleted
		}
	}()

	out := h.fixAsync(t.Context())
	<-entered
	close(release)
	got := <-out
	close(stop)
	<-readerDone
	if got.err != nil || got.result == nil || !got.result.Success {
		t.Fatalf("heal = %#v, error = %v; want success", got.result, got.err)
	}
	if !reflect.DeepEqual(h.entry, before) {
		t.Fatalf("the caller's entry changed: %+v, want %+v", h.entry, before)
	}
}
