package manager

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const droppedHash = "abcdef0123456789"

func newDroppedTestManager(t *testing.T, recovery ArrRecovery) (*Manager, *storage.Entry) {
	t.Helper()
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.NewStore(&config.Config{Arrs: []config.Arr{
		{Name: "sonarr", Host: "http://localhost:8989", Token: "x"},
	}})
	m := &Manager{
		clients:          xsync.NewMap[string, debrid.Client](),
		providerMisses:   xsync.NewMap[string, map[string]struct{}](),
		dropHookFailures: xsync.NewMap[string, map[string]int](),
		storage:          store,
		queue:            newQueue(store, "", nil, zerolog.Nop()),
		arr:              arr.New(cfg, nil, zerolog.Nop()),
		logger:           zerolog.Nop(),
	}
	if recovery != nil {
		m.SetArrRecovery(recovery)
	}
	entry := &storage.Entry{
		InfoHash: droppedHash,
		Name:     "Show.S01",
		Category: "sonarr",
		State:    storage.EntryStatePausedUP,
		Files:    map[string]*storage.File{"a.mkv": {ID: "file-1", Name: "a.mkv"}},
	}
	if addErr := m.queue.Add(entry); addErr != nil {
		t.Fatal(addErr)
	}
	return m, entry
}

func TestDroppedImportedEntryReacquiresBoundFiles(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{
		binding: reacquire.Binding{EntryID: droppedHash, EntryFileID: "file-1"},
		started: make(chan struct{}),
	}
	m, entry := newDroppedTestManager(t, fake)

	if err := m.recoverDroppedEntry("realdebrid", entry); err != nil {
		t.Fatal(err)
	}

	if fake.calls.Load() != 1 || fake.request.Cause != reacquire.CauseRepair {
		t.Fatalf("reacquire calls=%d request=%+v", fake.calls.Load(), fake.request)
	}
	if len(fake.fails) != 0 {
		t.Fatalf("unexpected FailDownload: %v", fake.fails)
	}
	record, err := m.Queue().GetTorrent(droppedHash)
	if err != nil || record.State != storage.EntryStatePausedUP {
		t.Fatalf("queue record changed: %v %v", record, err)
	}
}

func TestDroppedUnimportedEntryFailsGrab(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{}
	m, entry := newDroppedTestManager(t, fake)

	if err := m.recoverDroppedEntry("realdebrid", entry); err != nil {
		t.Fatal(err)
	}

	record, err := m.Queue().GetTorrent(droppedHash)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != storage.EntryStateError ||
		!strings.Contains(record.LastError, "realdebrid no longer lists this torrent") {
		t.Fatalf("record state=%s err=%q", record.State, record.LastError)
	}
	want := [3]string{"sonarr", strings.ToUpper(droppedHash), droppedHash}
	if len(fake.fails) != 1 || fake.fails[0] != want {
		t.Fatalf("fails=%v want %v", fake.fails, want)
	}
}

func TestDroppedEntryIsHandledOnce(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{}
	m, entry := newDroppedTestManager(t, fake)

	for range 2 {
		if err := m.recoverDroppedEntry("realdebrid", entry); err != nil {
			t.Fatal(err)
		}
	}

	record, err := m.Queue().GetTorrent(droppedHash)
	if err != nil || record.ErrorCount != 1 {
		t.Fatalf("record=%v err=%v, want ErrorCount 1", record, err)
	}
}

func TestDroppedEntryWithoutRecovery(t *testing.T) {
	t.Parallel()
	m, entry := newDroppedTestManager(t, nil)

	if err := m.recoverDroppedEntry("realdebrid", entry); err == nil {
		t.Fatal("want error when an Arr exists but the recovery service is not running")
	}
	record, err := m.Queue().GetTorrent(droppedHash)
	if err != nil || record.State == storage.EntryStateError {
		t.Fatalf("record=%v err=%v", record, err)
	}
}

func TestDroppedEntryKeptWhenFailDownloadErrors(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{failErr: errors.New("arr unreachable")}
	m, entry := newDroppedTestManager(t, fake)
	if err := m.storage.AddOrUpdate(entry); err != nil {
		t.Fatal(err)
	}

	for want := 1; want <= 2; want++ {
		m.handleTorrentDeletions("realdebrid", []*storage.Entry{entry})
		if _, err := m.storage.Get(droppedHash); err != nil {
			t.Fatalf("attempt %d: entry deleted despite failed recovery: %v", want, err)
		}
		if len(fake.fails) != want {
			t.Fatalf("attempt %d: FailDownload calls=%d", want, len(fake.fails))
		}
		record, err := m.Queue().GetTorrent(droppedHash)
		if err != nil || record.State == storage.EntryStateError {
			t.Fatalf("attempt %d: record marked errored: %v %v", want, record, err)
		}
	}
}

func TestDroppedEntryWithoutConfiguredArr(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{}
	m, entry := newDroppedTestManager(t, fake)
	record, err := m.Queue().GetTorrent(droppedHash)
	if err != nil {
		t.Fatal(err)
	}
	record.Category = "lidarr"
	if updateErr := m.Queue().Update(record); updateErr != nil {
		t.Fatal(updateErr)
	}
	if addErr := m.storage.AddOrUpdate(entry); addErr != nil {
		t.Fatal(addErr)
	}

	m.handleTorrentDeletions("realdebrid", []*storage.Entry{entry})

	record, err = m.Queue().GetTorrent(droppedHash)
	if err != nil || record.State != storage.EntryStateError {
		t.Fatalf("record=%v err=%v", record, err)
	}
	if len(fake.fails) != 0 {
		t.Fatalf("unexpected FailDownload: %v", fake.fails)
	}
	if _, getErr := m.storage.Get(droppedHash); getErr == nil {
		t.Fatal("entry was not deleted")
	}
}

func TestHandleTorrentDeletionsRunsHookBeforeDelete(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{}
	m, entry := newDroppedTestManager(t, fake)
	if err := m.storage.AddOrUpdate(entry); err != nil {
		t.Fatal(err)
	}

	m.handleTorrentDeletions("realdebrid", []*storage.Entry{entry})

	if _, err := m.storage.Get(droppedHash); err == nil {
		t.Fatal("entry was not deleted")
	}
	if len(fake.fails) != 1 {
		t.Fatalf("FailDownload calls=%d, want 1", len(fake.fails))
	}
}

func TestFailedHookKeepsStoredPlacement(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{failErr: errors.New("arr unreachable")}
	m, entry := newDroppedTestManager(t, fake)
	entry.Providers = map[string]*storage.ProviderEntry{"realdebrid": {Provider: "realdebrid", ID: "rd-1"}}
	if err := m.storage.AddOrUpdate(entry); err != nil {
		t.Fatal(err)
	}

	m.handleTorrentDeletions("realdebrid", []*storage.Entry{entry})

	got, err := m.storage.Get(droppedHash)
	if err != nil {
		t.Fatal(err)
	}
	if p := got.Providers["realdebrid"]; p == nil || p.ID != "rd-1" {
		t.Fatalf("stored placement lost: %+v", got.Providers)
	}
}

func TestFailedReacquireKeepsImportedEntry(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{
		binding:      reacquire.Binding{EntryID: droppedHash, EntryFileID: "file-1"},
		started:      make(chan struct{}),
		reacquireErr: errors.New("queue unreadable"),
	}
	m, entry := newDroppedTestManager(t, fake)
	if err := m.storage.AddOrUpdate(entry); err != nil {
		t.Fatal(err)
	}

	m.handleTorrentDeletions("realdebrid", []*storage.Entry{entry})

	if _, err := m.storage.Get(droppedHash); err != nil {
		t.Fatalf("imported entry deleted despite failed reacquire: %v", err)
	}
}

// Once its grab is failed in the Arr, a dropped download's queue record is
// removed so the Arr stops tracking a download that no longer exists.
func TestInvalidateDownloadFailedRemovesQueueRecord(t *testing.T) {
	t.Parallel()
	m, entry := newDroppedTestManager(t, &fakeArrRecovery{})
	job := reacquire.Job{Strategy: reacquire.StrategyDownloadFailed, EntryID: entry.InfoHash}

	if err := m.InvalidateReacquire(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if m.queue.Contains(entry.InfoHash) {
		t.Fatal("queue record still present after the grab was failed")
	}
	if err := m.InvalidateReacquire(t.Context(), job); err != nil {
		t.Fatalf("second invalidation of a removed record: %v", err)
	}
}

// Each confirmed drop leaves one INFO line naming the job that fails its grab.
func TestDroppedEntryLogsTheRecoveryJob(t *testing.T) {
	t.Parallel()
	m, entry := newDroppedTestManager(t, &fakeArrRecovery{})
	var logs bytes.Buffer
	m.logger = zerolog.New(&logs)
	if err := m.recoverDroppedEntry("torbox", entry); err != nil {
		t.Fatal(err)
	}
	line := logs.String()
	if !strings.Contains(line, "failing its grab in the Arr") || !strings.Contains(line, `"job":"fail-1"`) ||
		!strings.Contains(line, `"level":"info"`) {
		t.Fatalf("drop log = %s", line)
	}
}
