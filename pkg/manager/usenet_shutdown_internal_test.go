package manager

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet"
	"github.com/sirrobot01/decypharr/pkg/usenet/manifest"
	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
)

// processingNZBJob queues an NZB entry whose usenet meta is parsing, with its
// source file on disk, and returns the job that processes it.
func processingNZBJob(t *testing.T, m *Manager, hash string) (*Job, string) {
	t.Helper()
	source := filepath.Join(t.TempDir(), hash+".nzb")
	if err := os.WriteFile(source, []byte("<nzb/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	meta := &storage.NZB{ID: hash, Name: "Movie", Path: source, Status: usenet.NZBStatusParsing}
	if err := m.usenet.NZBStorage().AddNZB(meta); err != nil {
		t.Fatal(err)
	}
	entry := &storage.Entry{
		InfoHash: hash, Name: "Movie", Category: "sonarr", Protocol: config.ProtocolNZB,
		ActiveProvider: usenetProvider,
		State:          storage.EntryStateDownloading, Status: debridTypes.TorrentStatusDownloading,
	}
	if err := m.queue.Add(entry); err != nil {
		t.Fatal(err)
	}
	return &Job{
		ID: hash, Type: JobTypeNZB, Entry: entry, NZBMeta: meta,
		NZBGroups: map[string]*parser.FileGroup{"movie": {
			BaseName: "movie",
			Type:     storage.NZBFileTypeMedia,
			Files: []manifest.File{{
				Filename: "movie.mkv", Bytes: 100, TotalSegments: 1,
				Segments: []manifest.Segment{{Number: 1, Bytes: 100, MessageID: "a@b"}},
			}},
		}},
	}, source
}

// Shutdown cancels the job context while Process runs. Neither the usenet
// meta nor the entry may be failed: the entry would be reported to its arr and
// blocklisted, and a failed meta is never re-driven by the next start. The
// restore must then be able to rebuild the job, which needs the source file.
func TestShutdownDuringNZBProcessKeepsMetaParsingAndEntryActive(t *testing.T) {
	t.Parallel()
	m := newRestoreTestManager(t)
	m.usenet = newUnreachableUsenet(t)
	m.usenetTimeout = time.Minute
	const hash = "nzb-shutdown"
	job, source := processingNZBJob(t, m, hash)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	m.processJob(ctx, job)

	header, err := m.usenet.NZBStorage().GetNZBHeader(hash)
	if err != nil {
		t.Fatal(err)
	}
	if header.Status != usenet.NZBStatusParsing {
		t.Fatalf("meta status = %q after shutdown, want %q", header.Status, usenet.NZBStatusParsing)
	}
	stored, err := m.queue.GetTorrent(hash)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != storage.EntryStateDownloading {
		t.Fatalf("entry state = %q after shutdown, want %q", stored.State, storage.EntryStateDownloading)
	}
	if _, statErr := os.Stat(source); statErr != nil {
		t.Fatalf("source NZB lost by the interrupted run: %v", statErr)
	}
	if !m.nzbNeedsReprocessing(stored) {
		t.Fatal("the restore would not re-drive the interrupted NZB")
	}
	m.parseNZBFunc = func(context.Context, string, string, []byte, string) (*storage.NZB, map[string]*parser.FileGroup, error) {
		return &storage.NZB{ID: hash, Name: "Movie"}, nil, nil
	}
	if _, rebuildErr := m.rebuildQueuedNZBJob(stored); rebuildErr != nil {
		t.Fatalf("restore cannot rebuild the interrupted job: %v", rebuildErr)
	}
}

// A processing run that outlives its own budget is a real failure of that
// release, not a shutdown: the meta and the entry are both failed so it is
// neither retried forever nor left stuck.
func TestNZBProcessTimeoutFailsMetaAndEntry(t *testing.T) {
	t.Parallel()
	m := newRestoreTestManager(t)
	m.usenet = newUnreachableUsenet(t)
	m.usenetTimeout = time.Nanosecond
	const hash = "nzb-timeout"
	job, _ := processingNZBJob(t, m, hash)

	m.processJob(t.Context(), job)

	header, err := m.usenet.NZBStorage().GetNZBHeader(hash)
	if err != nil {
		t.Fatal(err)
	}
	if header.Status != usenet.NZBStatusFailed {
		t.Fatalf("meta status = %q after a timeout, want %q", header.Status, usenet.NZBStatusFailed)
	}
	stored, err := m.queue.GetTorrent(hash)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != storage.EntryStateError {
		t.Fatalf("entry state = %q after a timeout, want %q", stored.State, storage.EntryStateError)
	}
}
