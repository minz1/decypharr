package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/appendstore"
	"google.golang.org/protobuf/proto"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/kvstore"
	"github.com/sirrobot01/decypharr/pkg/arr"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet"
	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
)

func TestRestoreLeavesActiveDownloadsOutsideSubmissionWorkers(t *testing.T) {
	t.Parallel()
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := withTestConfig(
		t,
		&Manager{
			queue:  newQueue(store, "", nil, zerolog.Nop()),
			arr:    arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop()),
			logger: zerolog.Nop(),
			ctx:    t.Context(),
		},
	)
	for i := range 4 {
		status := debridTypes.TorrentStatusDownloading
		if i == 3 {
			status = debridTypes.TorrentStatusQueued
		}
		if addErr := m.queue.Add(&storage.Entry{
			InfoHash: fmt.Sprintf("%040d", i), Name: fmt.Sprintf("entry-%d", i),
			Protocol: config.ProtocolTorrent, State: storage.EntryStateDownloading,
			Status: status, IsDownloading: i != 3, AddedOn: time.Now().Add(time.Duration(i) * time.Second),
		}); addErr != nil {
			t.Fatal(addErr)
		}
	}
	received := make(chan *Job, 4)
	m.jobQueue = NewJobQueue(t.Context(), 1, func(ctx context.Context, job *Job) {
		if job.Request == nil {
			m.processJob(ctx, job)
		}
		received <- job
	}, zerolog.Nop())
	t.Cleanup(m.jobQueue.Close)
	m.restoreActiveDownloadJobs(m.restoreSnapshot())
	select {
	case job := <-received:
		if job.ID != fmt.Sprintf("%040d", 3) || job.Request == nil {
			t.Fatalf("restored a wait-only job: %#v", job)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active downloads blocked the queued import")
	}
	for i := range 3 {
		entry, getTorrentErr := m.queue.GetTorrent(fmt.Sprintf("%040d", i))
		if getTorrentErr != nil {
			t.Fatal(getTorrentErr)
		}
		if entry.Status != debridTypes.TorrentStatusDownloading || entry.IsDownloading {
			t.Fatalf("active entry = %#v", entry)
		}
	}
}

func TestRebuiltQueuedJobKeepsManual(t *testing.T) {
	t.Parallel()
	m := withTestConfig(t, &Manager{
		arr:    arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop()),
		logger: zerolog.Nop(),
	})
	for _, manual := range []bool{true, false} {
		entry := &storage.Entry{
			InfoHash: fmt.Sprintf("%040d", 1), Name: "entry", Protocol: config.ProtocolTorrent,
			Manual: manual, Status: debridTypes.TorrentStatusQueued,
		}
		job, err := m.rebuildQueuedTorrentJob(entry)
		if err != nil {
			t.Fatal(err)
		}
		if job.Request.Manual != manual {
			t.Fatalf("torrent rebuild Manual = %v, want %v", job.Request.Manual, manual)
		}
	}
}

// The restore runs while the API serves: it acts on the stored entry, not its snapshot copy.
func TestRestoreSkipsEntriesChangedSinceTheSnapshot(t *testing.T) {
	t.Parallel()
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := withTestConfig(t, &Manager{
		queue:  newQueue(store, "", nil, zerolog.Nop()),
		arr:    arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop()),
		logger: zerolog.Nop(),
		ctx:    t.Context(),
	})
	deleted, held, changed := fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2), fmt.Sprintf("%040d", 3)
	for _, hash := range []string{deleted, held, changed} {
		addRestoreTestEntry(t, m, hash)
	}
	snapshot, err := m.restoreSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	running, release := make(chan struct{}, 1), make(chan struct{})
	m.jobQueue = NewJobQueue(t.Context(), 1, func(context.Context, *Job) {
		running <- struct{}{}
		<-release
	}, zerolog.Nop())
	t.Cleanup(m.jobQueue.Close)
	t.Cleanup(func() { close(release) })
	if submitErr := m.jobQueue.Submit(&Job{ID: held}); submitErr != nil {
		t.Fatal(submitErr)
	}
	<-running
	if deleteErr := m.queue.Delete(deleted, false, nil); deleteErr != nil {
		t.Fatal(deleteErr)
	}
	renameStored(t, m, held, "fresh")
	renameStored(t, m, changed, "fresh")

	m.restoreActiveDownloadJobs(snapshot, nil)

	if m.queue.Contains(deleted) || m.jobQueue.FindJob(deleted) != nil {
		t.Fatal("a deleted entry was written back or resubmitted")
	}
	if m.jobQueue.FindJob(held) != nil {
		t.Fatal("an entry with a job was submitted again")
	}
	job := m.jobQueue.FindJob(changed)
	if job == nil || job.Entry.Name != "fresh" {
		t.Fatalf("changed entry job = %#v, want one built from the stored entry", job)
	}
	for _, hash := range []string{held, changed} {
		if entry, getErr := m.queue.GetTorrent(hash); getErr != nil || entry.Name != "fresh" {
			t.Fatalf("entry %s = %#v, error = %v: the snapshot copy was written back", hash, entry, getErr)
		}
	}
}

func addRestoreTestEntry(t *testing.T, m *Manager, hash string) {
	t.Helper()
	if err := m.queue.Add(&storage.Entry{
		InfoHash: hash, Name: "snapshot", Protocol: config.ProtocolTorrent, State: storage.EntryStateDownloading,
		Status: debridTypes.TorrentStatusQueued, IsDownloading: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func renameStored(t *testing.T, m *Manager, hash, name string) {
	t.Helper()
	entry, err := m.queue.GetTorrent(hash)
	if err != nil {
		t.Fatal(err)
	}
	entry.Name = name
	if updateErr := m.queue.Update(entry); updateErr != nil {
		t.Fatal(updateErr)
	}
}

// A flag set after the snapshot belongs to a live download; only the previous run's flag is reset.
func TestRestoreKeepsAFlagSetSinceTheSnapshot(t *testing.T) {
	t.Parallel()
	m := newRestoreTestManager(t)
	hash := fmt.Sprintf("%040d", 1)
	if err := m.queue.Add(&storage.Entry{
		InfoHash: hash, Name: "live", Protocol: config.ProtocolTorrent, State: storage.EntryStateDownloading,
		Status: debridTypes.TorrentStatusDownloading,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.restoreSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	live, err := m.queue.GetTorrent(hash)
	if err != nil {
		t.Fatal(err)
	}
	live.IsDownloading = true
	if updateErr := m.queue.Update(live); updateErr != nil {
		t.Fatal(updateErr)
	}

	m.restoreActiveDownloadJobs(snapshot, nil)

	if saved, getErr := m.queue.GetTorrent(hash); getErr != nil || !saved.IsDownloading {
		t.Fatalf("live entry = %#v, error = %v: the restore reset a running download", saved, getErr)
	}
}

func newRestoreTestManager(t *testing.T) *Manager {
	t.Helper()
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := withTestConfig(t, &Manager{
		queue:  newQueue(store, "", nil, zerolog.Nop()),
		arr:    arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop()),
		logger: zerolog.Nop(),
		ctx:    t.Context(),
	})
	m.jobQueue = NewJobQueue(t.Context(), 1, func(context.Context, *Job) {}, zerolog.Nop())
	t.Cleanup(m.jobQueue.Close)
	return m
}

// A restart during a busy batch must not blocklist a release whose restore parse ran out of time.
func TestRestoreRequeuesAnNZBWhoseParseRanOutOfTime(t *testing.T) {
	t.Parallel()
	m := newRestoreTestManager(t)
	m.jobQueue.Close()
	m.usenet = newUnreachableUsenet(t)
	m.usenetTimeout = 20 * time.Millisecond
	m.parseRequeueDelay = 20 * time.Millisecond
	type parseCall struct{ name, category string }
	calls := make(chan parseCall, 8)
	var n atomic.Int32
	m.parseNZBFunc = func(ctx context.Context, _, name string, _ []byte, category string) (*storage.NZB, map[string]*parser.FileGroup, error) {
		select {
		case calls <- parseCall{name, category}:
		default:
		}
		if n.Add(1) < 3 {
			<-ctx.Done()
			return nil, nil, ctx.Err()
		}
		return &storage.NZB{Name: name}, nil, nil
	}
	m.jobQueue = NewJobQueue(t.Context(), 1, m.processJob, zerolog.Nop())
	t.Cleanup(m.jobQueue.Close)
	staged := filepath.Join(t.TempDir(), "obf.nzb")
	if err := os.WriteFile(staged, []byte("<nzb/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash := "nzb-restored"
	if err := m.queue.Add(&storage.Entry{
		InfoHash: hash, Name: "obf", OriginalFilename: "Show.S01.1080p", Category: "sonarr",
		Protocol: config.ProtocolNZB, State: storage.EntryStateDownloading, Status: debridTypes.TorrentStatusQueued,
		Magnet: staged, SavePath: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}

	m.restoreActiveDownloadJobs(m.restoreSnapshot())

	for range 3 {
		select {
		case got := <-calls:
			if got != (parseCall{"Show.S01.1080p", "sonarr"}) {
				t.Fatalf("parse got %+v, want the entry's original name and category", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the requeued parse never ran")
		}
	}
	eventually(t, "the requeued job to finish", func() bool { return !m.jobQueue.Holds(hash) })
	if entry, err := m.queue.GetTorrent(hash); err != nil || entry.ActiveProvider != usenetProvider {
		t.Fatalf("entry = %+v, %v: want it parsed by the requeued job", entry, err)
	}
}

// Without a staged NZB the requeued job would have nothing to parse, so a re-processing restore fails at once.
func TestRestoreFailsAReprocessedNZBWhoseParseRanOutOfTime(t *testing.T) {
	t.Parallel()
	m := newRestoreTestManager(t)
	m.usenet = newUnreachableUsenet(t)
	m.usenetTimeout = 20 * time.Millisecond
	m.parseRequeueDelay = time.Hour
	var calls atomic.Int32
	m.parseNZBFunc = func(ctx context.Context, _, _ string, _ []byte, _ string) (*storage.NZB, map[string]*parser.FileGroup, error) {
		calls.Add(1)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	saved := filepath.Join(t.TempDir(), "saved.nzb")
	if err := os.WriteFile(saved, []byte("<nzb/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash := "nzb-reprocessed"
	header := &storage.NZB{ID: hash, Status: usenet.NZBStatusDownloading, Path: saved}
	if err := m.usenet.NZBStorage().AddNZB(header); err != nil {
		t.Fatal(err)
	}
	if err := m.queue.Add(&storage.Entry{
		InfoHash: hash, Name: "Show.S01", Category: "sonarr", Protocol: config.ProtocolNZB,
		State: storage.EntryStateDownloading, Status: debridTypes.TorrentStatusDownloading,
	}); err != nil {
		t.Fatal(err)
	}

	m.restoreActiveDownloadJobs(m.restoreSnapshot())

	entry, err := m.queue.GetTorrent(hash)
	if err != nil || entry.State != storage.EntryStateError || calls.Load() != 2 || m.jobQueue.Holds(hash) {
		t.Fatalf("entry = %+v, %v after %d calls: want it failed by the restore", entry, err, calls.Load())
	}
}

// A restored entry whose reset cannot be saved still gets its job; the job's own writes clear the flag.
func TestRestoreResubmitsAnEntryWhoseResetCannotBeSaved(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "db")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%040d", 1)
	// The oversized name fits a raw record but not the attributes every queue write adds.
	data, err := proto.Marshal(storage.EntryToProto(&storage.Entry{
		InfoHash: hash, Name: strings.Repeat("n", 16<<20), Category: "radarr", Protocol: config.ProtocolTorrent,
		State: storage.EntryStateDownloading, Status: debridTypes.TorrentStatusQueued, IsDownloading: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := kvstore.Open(filepath.Join(dir, "queue.db"), appendstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = raw.Put(hash, data, nil); err != nil {
		t.Fatal(err)
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewStorage(dir, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := withTestConfig(t, &Manager{
		queue:  newQueue(store, "", nil, zerolog.Nop()),
		logger: zerolog.Nop(),
		ctx:    t.Context(),
	})
	m.jobQueue = NewJobQueue(t.Context(), 1, func(ctx context.Context, _ *Job) { <-ctx.Done() }, zerolog.Nop())
	t.Cleanup(m.jobQueue.Close)

	m.restoreActiveDownloadJobs(m.restoreSnapshot())

	if !m.jobQueue.Holds(hash) {
		t.Fatal("an entry whose reset could not be saved got no job")
	}
}

// An entry the restore cannot read may still be Queued; the "queued with no job" sweep must not remove it.
func TestRestoreReadErrorKeepsTheQueuedSweepOff(t *testing.T) {
	t.Parallel()
	m := newRestoreTestManager(t)
	addRestoreTestEntry(t, m, fmt.Sprintf("%040d", 1))
	snapshot, err := m.restoreSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := m.queue.storage.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	m.restoreActiveDownloadJobs(snapshot, nil)

	if m.queueRestored.Load() {
		t.Fatal("queueRestored = true after an entry could not be read")
	}
}
