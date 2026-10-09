package manager

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
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
		storage: store,
		queue:   newQueue(store, "", nil, zerolog.Nop()),
		arr:     arr.New(cfg, nil, zerolog.Nop()),
		logger:  zerolog.Nop(),
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

	m.recoverDroppedEntry("realdebrid", entry)

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

	m.recoverDroppedEntry("realdebrid", entry)

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

	m.recoverDroppedEntry("realdebrid", entry)
	m.recoverDroppedEntry("realdebrid", entry)

	if len(fake.fails) != 1 {
		t.Fatalf("FailDownload calls=%d, want 1", len(fake.fails))
	}
}

func TestDroppedEntryWithoutRecovery(t *testing.T) {
	t.Parallel()
	m, entry := newDroppedTestManager(t, nil)
	m.SetArrRecovery(nil)

	m.recoverDroppedEntry("realdebrid", entry)

	record, err := m.Queue().GetTorrent(droppedHash)
	if err != nil || record.State != storage.EntryStateError {
		t.Fatalf("record=%v err=%v", record, err)
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
