package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestQueueDeleteRemovesFiles(t *testing.T) {
	t.Parallel()
	queue, entry, downloadedPath := newQueueDeleteTest(t)

	if err := queue.deleteEntryFiles(entry); err != nil {
		t.Fatalf("delete entry files: %v", err)
	}
	if _, err := os.Stat(downloadedPath); !os.IsNotExist(err) {
		t.Fatalf("expected downloaded files to be removed, got %v", err)
	}
}

// A record without an absolute save path names no download: deleting its files
// once removed a directory relative to the process's working directory.
func TestQueueDeleteKeepsARelativeDownloadPath(t *testing.T) {
	t.Parallel()
	queue, entry, downloadedPath := newQueueDeleteTest(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, entry.SavePath)
	if err != nil {
		t.Fatal(err)
	}
	if err = queue.deleteEntryFiles(
		&storage.Entry{InfoHash: entry.InfoHash, Name: entry.Name, SavePath: rel},
	); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(downloadedPath); err != nil {
		t.Fatalf("a relative download path was deleted: %v", err)
	}
}

func newQueueDeleteTest(t *testing.T) (*Queue, *storage.Entry, string) {
	t.Helper()

	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("close storage: %v", closeErr)
		}
	})

	savePath := filepath.Join(t.TempDir(), "downloads")
	entry := &storage.Entry{
		InfoHash: "0123456789abcdef",
		Name:     "Example.mkv",
		SavePath: savePath,
	}
	downloadedPath := entry.DownloadPath("")
	if mkdirAllErr := os.MkdirAll(downloadedPath, 0o755); mkdirAllErr != nil {
		t.Fatalf("create downloaded path: %v", mkdirAllErr)
	}
	if writeFileErr := os.WriteFile(
		filepath.Join(downloadedPath, "video.mkv"),
		[]byte("test"),
		0o644,
	); writeFileErr != nil {
		t.Fatalf("create downloaded file: %v", writeFileErr)
	}

	queue := newQueue(store, nil, &grabIndex{}, zerolog.Nop())
	if addErr := queue.Add(entry); addErr != nil {
		t.Fatalf("add queued entry: %v", addErr)
	}
	return queue, entry, downloadedPath
}

// Removing a failed NZB entry must remove its staged file too; it once leaked one .queued file per failure.
func TestCleanupRemovesStagedNZB(t *testing.T) {
	t.Parallel()
	staged := filepath.Join(t.TempDir(), "job.queued")
	if writeErr := os.WriteFile(staged, []byte("<nzb/>"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	var buf bytes.Buffer
	m := &Manager{queue: &Queue{logger: zerolog.New(&buf)}, logger: zerolog.Nop()}
	entry := &storage.Entry{InfoHash: "job", Name: "failed", Protocol: config.ProtocolNZB, Magnet: staged}

	got := m.cleanupChild(t.Context(), entryChildCall{kind: grab.ChildCleanup, grab: grab.Grab{Entry: entry}})
	if got.ev == nil || got.ev.Kind != grab.EvCleanupDone {
		t.Fatalf("cleanup result %+v, want done", got)
	}
	if _, statErr := os.Stat(staged); !os.IsNotExist(statErr) {
		t.Fatalf("staged NZB still on disk after the cleanup: %v", statErr)
	}
	logged := decodeLogLines(t, &buf)
	if len(logged) != 1 || logged[0]["level"] != "info" || logged[0]["key"] != "job" || logged[0]["path"] != staged {
		t.Fatalf("staged-path log = %v", logged)
	}
}

// An entry whose grab is cleaned up forgets its provider wait: the map stays bounded, and a later grab of the same
// hash logs its wait's start at WARN again.
func TestCleanupForgetsTheProviderWait(t *testing.T) {
	t.Parallel()
	m := &Manager{queue: newQueue(nil, nil, &grabIndex{}, zerolog.Nop()), logger: zerolog.Nop()}
	entry := &storage.Entry{InfoHash: "0123456789ABCDEF0123456789ABCDEF01234567", Name: "converting"}
	m.queue.waits.observe(entry.InfoHash, waitConverting)

	m.cleanupChild(t.Context(), entryChildCall{kind: grab.ChildCleanup, grab: grab.Grab{Entry: entry}})
	if got := m.queue.waits.observe(entry.InfoHash, waitConverting); got != waitNone {
		t.Fatalf("after the cleanup the entry's wait is still %s, want it forgotten", got)
	}
}

func deleteTestQueue(naming config.WebDavFolderNaming, w io.Writer) *Queue {
	return newQueue(nil, func() config.WebDavFolderNaming { return naming }, &grabIndex{}, zerolog.New(w))
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func requirePresent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%q should exist: %v", path, err)
	}
}

func requireGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%q should be gone: %v", path, err)
	}
}

func decodeLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range bytes.SplitSeq(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		rec := map[string]any{}
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestDeletingOneOfTwoSameNamedGrabsKeepsTheOther(t *testing.T) {
	t.Parallel()
	for _, naming := range []config.WebDavFolderNaming{config.WebDavUseArrSubmittedName, config.WebDavUseFileName} {
		build := map[string]func(dl, id string) *storage.Entry{
			"nzb": func(dl, id string) *storage.Entry {
				return newNZBQueueEntry(&ImportRequest{
					ID: id, Name: "Show.S01E01", DownloadFolder: dl, Arr: arr.Arr{Name: "sonarr"},
				}, "")
			},
			"torrent": func(dl, id string) *storage.Entry {
				return newTorrentQueueEntry(&ImportRequest{
					Magnet:         &utils.Magnet{InfoHash: id, Name: "Show.S01E01"},
					DownloadFolder: dl, Arr: arr.Arr{Name: "sonarr"},
				}, storage.EntryStatusQueued, naming)
			},
		}
		for kind, newEntry := range build {
			t.Run(string(naming)+"/"+kind, func(t *testing.T) {
				t.Parallel()
				dl := t.TempDir()
				a, b := newEntry(dl, "aaaa"), newEntry(dl, "bbbb")
				fileA := filepath.Join(a.DownloadPath(naming), "ep.mkv")
				fileB := filepath.Join(b.DownloadPath(naming), "ep.mkv")
				writeTestFile(t, fileA)
				writeTestFile(t, fileB)
				if err := deleteTestQueue(naming, io.Discard).deleteEntryFiles(a); err != nil {
					t.Fatal(err)
				}
				requireGone(t, a.SavePath)
				requirePresent(t, fileB)
			})
		}
	}
	t.Run("nameless torrent", func(t *testing.T) {
		t.Parallel()
		dl := t.TempDir()
		e := newTorrentQueueEntry(&ImportRequest{
			Magnet: &utils.Magnet{InfoHash: "cccc"}, DownloadFolder: dl, Arr: arr.Arr{Name: "sonarr"},
		}, storage.EntryStatusQueued, config.WebDavUseArrSubmittedName)
		writeTestFile(t, filepath.Join(e.SavePath, "stray.mkv"))
		writeTestFile(t, filepath.Join(e.DownloadPath(config.WebDavUseArrSubmittedName), "ep.mkv"))
		if err := deleteTestQueue(config.WebDavUseArrSubmittedName, io.Discard).deleteEntryFiles(e); err != nil {
			t.Fatal(err)
		}
		requireGone(t, e.SavePath)
	})
}

func TestOwnedFolderIsRemovedWholeAfterANamingChange(t *testing.T) {
	t.Parallel()
	dl := t.TempDir()
	e := newTorrentQueueEntry(&ImportRequest{
		Magnet: &utils.Magnet{
			InfoHash: "0123456789abcdef0123456789abcdef01234567", Name: "File.Name.mkv",
			Link: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Arr.Name",
		},
		DownloadFolder: dl, Arr: arr.Arr{Name: "sonarr"},
	}, storage.EntryStatusQueued, config.WebDavUseFileName)
	written := e.DownloadPath(config.WebDavUseFileName)
	deleting := e.DownloadPath(config.WebDavUseArrSubmittedName)
	if written == deleting {
		t.Fatalf("both namings give %q", written)
	}
	writeTestFile(t, filepath.Join(written, "ep.mkv"))
	if err := deleteTestQueue(config.WebDavUseArrSubmittedName, io.Discard).deleteEntryFiles(e); err != nil {
		t.Fatal(err)
	}
	requireGone(t, e.SavePath)
}

func TestRecategorizedEntryStillRemovesItsFolder(t *testing.T) {
	t.Parallel()
	naming := config.WebDavUseArrSubmittedName
	e := newTorrentQueueEntry(&ImportRequest{
		Magnet:         &utils.Magnet{InfoHash: "aaaa", Name: "Show"},
		DownloadFolder: t.TempDir(), Arr: arr.Arr{Name: "sonarr"},
	}, storage.EntryStatusQueued, naming)
	writeTestFile(t, filepath.Join(e.DownloadPath(naming), "ep.mkv"))
	e.Category = "sonarr-imported"
	if err := deleteTestQueue(naming, io.Discard).deleteEntryFiles(e); err != nil {
		t.Fatal(err)
	}
	requireGone(t, e.SavePath)
}

func TestLegacyEntryNeverRemovesItsCategoryDir(t *testing.T) {
	t.Parallel()
	const h = "0123456789abcdef0123456789abcdef01234567"
	tmp := t.TempDir()
	e := &storage.Entry{InfoHash: h, Name: "Show", Category: h, SavePath: filepath.Join(tmp, h)}
	writeTestFile(t, filepath.Join(e.DownloadPath(""), "ep.mkv"))
	other := filepath.Join(tmp, h, "other.mkv")
	writeTestFile(t, other)
	if err := deleteTestQueue("", io.Discard).deleteEntryFiles(e); err != nil {
		t.Fatal(err)
	}
	requireGone(t, e.DownloadPath(""))
	requirePresent(t, other)
}

func TestOwnedSymlinkIsUnlinkedNotFollowed(t *testing.T) {
	t.Parallel()
	newOwned := func(t *testing.T) (*storage.Entry, string) {
		dl := t.TempDir()
		e := &storage.Entry{
			InfoHash: "aaaa",
			Name:     "Show",
			Category: "sonarr",
			SavePath: storage.EntrySavePath(filepath.Join(dl, "sonarr"), "aaaa"),
		}
		if err := os.MkdirAll(filepath.Dir(e.SavePath), 0o755); err != nil {
			t.Fatal(err)
		}
		return e, dl
	}
	t.Run("to a directory", func(t *testing.T) {
		t.Parallel()
		e, dl := newOwned(t)
		keep := filepath.Join(dl, "elsewhere", "keep.mkv")
		writeTestFile(t, keep)
		if err := os.Symlink(filepath.Dir(keep), e.SavePath); err != nil {
			t.Fatal(err)
		}
		if err := deleteTestQueue("", io.Discard).deleteEntryFiles(e); err != nil {
			t.Fatal(err)
		}
		requireGone(t, e.SavePath)
		requirePresent(t, keep)
	})
	t.Run("dangling, stored with a trailing separator", func(t *testing.T) {
		t.Parallel()
		e, dl := newOwned(t)
		if err := os.Symlink(filepath.Join(dl, "missing"), e.SavePath); err != nil {
			t.Fatal(err)
		}
		link := e.SavePath
		e.SavePath += string(filepath.Separator)
		if err := deleteTestQueue("", io.Discard).deleteEntryFiles(e); err != nil {
			t.Fatal(err)
		}
		requireGone(t, link)
	})
	t.Run("a regular file", func(t *testing.T) {
		t.Parallel()
		e, _ := newOwned(t)
		writeTestFile(t, e.SavePath)
		if err := deleteTestQueue("", io.Discard).deleteEntryFiles(e); err != nil {
			t.Fatal(err)
		}
		requireGone(t, e.SavePath)
	})
}

func TestDeletingAnAlreadyGoneOwnedFolderSucceeds(t *testing.T) {
	t.Parallel()
	naming := config.WebDavUseArrSubmittedName
	e := newTorrentQueueEntry(&ImportRequest{
		Magnet:         &utils.Magnet{InfoHash: "aaaa", Name: "Show"},
		DownloadFolder: t.TempDir(), Arr: arr.Arr{Name: "sonarr"},
	}, storage.EntryStatusQueued, naming)
	writeTestFile(t, filepath.Join(e.DownloadPath(naming), "ep.mkv"))
	q := deleteTestQueue(naming, io.Discard)
	if err := q.deleteEntryFiles(e); err != nil {
		t.Fatal(err)
	}
	requireGone(t, e.SavePath)
	if err := q.deleteEntryFiles(e); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestDeleteLogsThePath(t *testing.T) {
	t.Parallel()
	naming := config.WebDavUseArrSubmittedName
	staged := filepath.Join(t.TempDir(), "job.queued")
	writeTestFile(t, staged)
	e := newNZBQueueEntry(&ImportRequest{
		ID: "job-1", Name: "Show", DownloadFolder: t.TempDir(), Arr: arr.Arr{Name: "sonarr"},
	}, staged)
	var buf bytes.Buffer
	if err := deleteTestQueue(naming, &buf).deleteEntryFiles(e); err != nil {
		t.Fatal(err)
	}
	var download, stagedLine map[string]any
	for _, rec := range decodeLogLines(t, &buf) {
		if rec["level"] != "info" {
			t.Fatalf("record %v is not info", rec)
		}
		switch rec["message"] {
		case "Removing a deleted grab's files":
			download = rec
		case "Removing a staged NZB":
			stagedLine = rec
		}
	}
	if download == nil || download["name"] != "Show" || download["key"] != "job-1" ||
		download["path"] != e.SavePath || download["owned"] != true {
		t.Fatalf("download record = %v", download)
	}
	if stagedLine == nil || stagedLine["key"] != "job-1" || stagedLine["path"] != staged {
		t.Fatalf("staged record = %v", stagedLine)
	}
}

func TestDeleteLogsTheLegacyPathItRemoves(t *testing.T) {
	t.Parallel()
	e := &storage.Entry{
		InfoHash: "job-2",
		Name:     "Show",
		Category: "sonarr",
		SavePath: filepath.Join(t.TempDir(), "sonarr"),
	}
	var buf bytes.Buffer
	if err := deleteTestQueue("", &buf).deleteEntryFiles(e); err != nil {
		t.Fatal(err)
	}
	logged := decodeLogLines(t, &buf)
	if len(logged) != 1 || logged[0]["path"] != e.DownloadPath("") || logged[0]["owned"] != false {
		t.Fatalf("legacy delete log = %v, want path %q owned false", logged, e.DownloadPath(""))
	}
}

func TestLegacyDeleteUsesTheConfiguredNaming(t *testing.T) {
	t.Parallel()
	const h = "0123456789abcdef0123456789abcdef01234567"
	e := &storage.Entry{
		InfoHash: h,
		Name:     "File.Name.mkv",
		Magnet:   "magnet:?xt=urn:btih:" + h + "&dn=Arr.Name",
		Category: "sonarr",
		SavePath: filepath.Join(t.TempDir(), "sonarr"),
	}
	configured := config.WebDavUseArrSubmittedName
	other := config.WebDavUseFileName
	if e.DownloadPath(configured) == e.DownloadPath(other) {
		t.Fatal("both namings give one path")
	}
	writeTestFile(t, filepath.Join(e.DownloadPath(configured), "ep.mkv"))
	writeTestFile(t, filepath.Join(e.DownloadPath(other), "ep.mkv"))
	if err := deleteTestQueue(configured, io.Discard).deleteEntryFiles(e); err != nil {
		t.Fatal(err)
	}
	requireGone(t, e.DownloadPath(configured))
	requirePresent(t, filepath.Join(e.DownloadPath(other), "ep.mkv"))
}
