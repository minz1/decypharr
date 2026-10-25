//go:build !windows

package manager

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet"
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

// A re-parse that succeeds after the arr deleted the entry saved the NZB, its marker and its header; they go too.
func TestRestoreRemovesAnNZBParsedAfterItsEntryWasDeleted(t *testing.T) {
	t.Parallel()
	server, err := nntpd.New(nntpd.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	payload := make([]byte, 1024)
	copy(payload, []byte{0x1A, 0x45, 0xDF, 0xA3})
	server.AddArticle("<media@nntpd>", nntpd.Encode(payload, "Release.mkv", 1, int64(len(payload)), 0))
	host, port := server.Addr()
	cfgStore := testConfigStore(t, func(cfg *config.Config) {
		cfg.Usenet = config.Usenet{
			Providers:                []config.UsenetProvider{{Host: host, Port: port, MaxConnections: 2}},
			MaxConnections:           2,
			ProcessingMaxConnections: 2,
		}
	})
	usenetClient, err := usenet.New(cfgStore, logger.Discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = usenetClient.Close() })
	m := newRestoreTestManager(t)
	m.usenet = usenetClient
	m.usenetTimeout = 10 * time.Second
	source := filepath.Join(t.TempDir(), "release.nzb")
	if err = syscall.Mkfifo(source, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := "nzb-parsed-after-delete"
	if err = m.queue.Add(&storage.Entry{
		InfoHash: hash, Name: "Release", Protocol: config.ProtocolNZB, State: storage.EntryStateDownloading,
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
	writer, err := os.OpenFile(source, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if deleteErr := m.queue.Delete(hash, false, nil); deleteErr != nil {
		t.Fatal(deleteErr)
	}
	_, _ = writer.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="poster" date="1" subject="Release.mkv [1/1] yEnc (1/1)">
    <groups><group>alt.test</group></groups>
    <segments><segment bytes="1100" number="1">media@nntpd</segment></segments>
  </file>
</nzb>`)
	_ = writer.Close()
	<-done

	if header, getErr := usenetClient.GetNZBHeader(hash); getErr == nil {
		t.Fatalf("usenet header = %+v, want none for a deleted entry", header)
	}
	left, err := filepath.Glob(filepath.Join(cfgStore.Get().Dir(), "usenet", "nzbs", hash+"*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("files left for a deleted entry: %v", left)
	}
}
