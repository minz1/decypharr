package repair

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const otherHash = "1234512345123451234512345123451234512345"

func clearService(t *testing.T, entries ...*storage.Entry) (*Service, *clientBackend) {
	t.Helper()
	store := newRepairTestStorage(t)
	for _, entry := range entries {
		if err := store.AddOrUpdate(entry); err != nil {
			t.Fatal(err)
		}
	}
	backend := &clientBackend{client: newLinkClient(nil)}
	return New(Dependencies{Storage: store, Backend: backend}), backend
}

func healthy(hash, name string) fileResult {
	return fileResult{name: name, infoHash: hash, protocol: config.ProtocolTorrent, healthy: true, fresh: true}
}

func markedEntry(hash string, names ...string) *storage.Entry {
	entry := confirmEntry(names...)
	entry.InfoHash = hash
	entry.Bad = true
	for _, file := range entry.Files {
		file.InfoHash = hash
	}
	return entry
}

func TestClearDisprovedMarksNeedsEveryFileHealthy(t *testing.T) {
	t.Parallel()
	brokenB := healthy(confirmHash, "b.mkv")
	brokenB.healthy, brokenB.broken = false, true
	inconclusiveB := healthy(confirmHash, "b.mkv")
	inconclusiveB.healthy, inconclusiveB.reason = false, "unrestrict_link_error"
	cancelledB := healthy(confirmHash, "b.mkv")
	cancelledB.healthy, cancelledB.reason = false, "context_cancelled"
	a := healthy(confirmHash, "a.mkv")
	for _, tc := range []struct {
		name    string
		results []fileResult
		want    int
	}{
		{name: "all healthy", results: []fileResult{a, healthy(confirmHash, "b.mkv")}, want: 1},
		{name: "one broken", results: []fileResult{a, brokenB}},
		{name: "one inconclusive", results: []fileResult{a, inconclusiveB}},
		{name: "one cancelled", results: []fileResult{a, cancelledB}},
		{name: "one missing", results: []fileResult{a}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			service, backend := clearService(t, markedEntry(confirmHash, "a.mkv", "b.mkv"))
			service.clearDisprovedMarks(t.Context(), tc.results)
			if len(backend.cleared) != tc.want {
				t.Fatalf("ClearBad calls = %v, want %d", backend.cleared, tc.want)
			}
		})
	}
}

// Each infohash is judged on its own files, even when another one shares a file name.
func TestClearDisprovedMarksJudgesOwnFiles(t *testing.T) {
	t.Parallel()
	service, backend := clearService(t, markedEntry(confirmHash, "x.mkv"), markedEntry(otherHash, "x.mkv"))
	service.clearDisprovedMarks(t.Context(), []fileResult{healthy(otherHash, "x.mkv")})
	if len(backend.cleared) != 1 || backend.cleared[0] != otherHash {
		t.Fatalf("ClearBad calls = %v, want only %s", backend.cleared, otherHash)
	}
}

func TestClearDisprovedMarksNeverClearsNZB(t *testing.T) {
	t.Parallel()
	entry := markedEntry(confirmHash, "a.mkv")
	entry.Protocol = config.ProtocolNZB
	service, backend := clearService(t, entry)
	service.clearDisprovedMarks(t.Context(), []fileResult{healthy(confirmHash, "a.mkv")})
	if len(backend.cleared) != 0 {
		t.Fatalf("ClearBad calls = %v, want none for an NZB entry", backend.cleared)
	}
}

func probeOnce(t *testing.T, entry *storage.Entry) *clientBackend {
	t.Helper()
	service, backend := clearService(t, entry)
	item := &storage.EntryItem{Name: entry.Name, Files: entry.Files}
	service.probeEntry(t.Context(), "run", &candidate{name: entry.Name, item: item}, newErrorCache(), nil,
		RunOptions{}, false)
	return backend
}

func TestProbeEntryClearsDisprovedMark(t *testing.T) {
	t.Parallel()
	if backend := probeOnce(t, markedEntry(confirmHash, "a.mkv", "b.mkv")); len(backend.cleared) != 1 {
		t.Fatalf("ClearBad calls = %v, want one", backend.cleared)
	}
}

func TestProbeEntryLeavesUnmarkedAlone(t *testing.T) {
	t.Parallel()
	entry := markedEntry(confirmHash, "a.mkv")
	entry.Bad = false
	if backend := probeOnce(t, entry); len(backend.cleared) != 0 {
		t.Fatalf("ClearBad calls = %v, want none", backend.cleared)
	}
}

// A light CheckFile pass never clears a mark: only the fresh read check that set it may.
func TestClearDisprovedMarksNeedsFreshResults(t *testing.T) {
	t.Parallel()
	light := healthy(confirmHash, "a.mkv")
	light.fresh = false
	service, backend := clearService(t, markedEntry(confirmHash, "a.mkv"))
	service.clearDisprovedMarks(t.Context(), []fileResult{light})
	if len(backend.cleared) != 0 {
		t.Fatalf("ClearBad calls = %v, want none from a light probe", backend.cleared)
	}
}
