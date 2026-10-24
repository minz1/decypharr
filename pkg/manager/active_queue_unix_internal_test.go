//go:build !windows

package manager

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// An NZB re-parse can take minutes: an entry the arr deletes meanwhile is not written back.
func TestRestoreLeavesAnNZBDeletedDuringItsRebuild(t *testing.T) {
	t.Parallel()
	m := newRestoreTestManager(t)
	m.usenet = newUnreachableUsenet(t)
	source := filepath.Join(t.TempDir(), "release.nzb")
	if err := syscall.Mkfifo(source, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := "nzb-deleted-during-rebuild"
	if err := m.queue.Add(&storage.Entry{
		InfoHash: hash, Name: "release", Protocol: config.ProtocolNZB, State: storage.EntryStateDownloading,
		Status: debridTypes.TorrentStatusQueued, Magnet: source,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.restoreSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.restoreActiveDownloadJobs(snapshot, nil)
	}()
	// Opening the FIFO for writing returns once the rebuild is reading it.
	writer, err := os.OpenFile(source, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if deleteErr := m.queue.Delete(hash, false, nil); deleteErr != nil {
		t.Fatal(deleteErr)
	}
	_ = writer.Close()
	<-done

	if m.queue.Contains(hash) || m.jobQueue.Holds(hash) {
		t.Fatal("an NZB deleted during its rebuild was written back or resubmitted")
	}
}
