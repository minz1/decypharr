package manager

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
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
	running, release := make(chan struct{}), make(chan struct{})
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
