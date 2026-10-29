package repair

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const sweepEntryHash = "0123456789012345678901234567890123456789"

// sweepFixture is one arr-mapped torrent entry whose provider has no links for
// its files, so every probe finds them broken (missing_provider_link).
type sweepFixture struct {
	store      *storage.Storage
	service    *Service
	reacquirer *fakeReacquirer
	backend    *removedProviderBackend
	entry      *storage.Entry
	run        *storage.RepairRun
}

func newSweepFixture(t *testing.T, fileNames ...string) *sweepFixture {
	t.Helper()
	store := newRepairTestStorage(t)
	entry := &storage.Entry{
		InfoHash: sweepEntryHash, Name: "release", Protocol: config.ProtocolTorrent, ActiveProvider: "removed",
		Files: map[string]*storage.File{},
		Providers: map[string]*storage.ProviderEntry{
			"removed": {Provider: "removed", ID: "old", Status: types.TorrentStatusDownloaded},
		},
	}
	for _, name := range fileNames {
		entry.Files[name] = &storage.File{ID: "id-" + name, Name: name, InfoHash: sweepEntryHash}
	}
	if err := store.AddOrUpdate(entry); err != nil {
		t.Fatal(err)
	}
	reacquirer := &fakeReacquirer{reacquire: func(reacquire.Request) (*reacquire.Job, error) {
		return &reacquire.Job{ID: "job"}, nil
	}}
	backend := &removedProviderBackend{
		reinsert: func(*storage.Entry) error { return errors.New("no replacement") },
		client:   newLinkClient(nil),
	}
	service := New(Dependencies{Storage: store, Backend: backend, Reacquirer: reacquirer})
	return &sweepFixture{
		store: store, service: service, reacquirer: reacquirer, backend: backend, entry: entry,
		run: &storage.RepairRun{ID: "run"},
	}
}

func (f *sweepFixture) candidates() map[string]*candidate {
	content := make(map[string]arr.ContentFile, len(f.entry.Files))
	for name := range f.entry.Files {
		content[name] = arr.ContentFile{Name: name, Path: "/library/" + name, ID: 9, FileID: 7}
	}
	return map[string]*candidate{f.entry.Name: {
		name: f.entry.Name, item: &storage.EntryItem{Name: f.entry.Name, Files: f.entry.Files},
		arrName: "radarr", arrKind: storage.ArrKindRadarr, contentMap: content,
	}}
}

func (f *sweepFixture) probe(ctx context.Context, t *testing.T, confirm bool) map[string]*candidate {
	t.Helper()
	candidates := f.candidates()
	err := f.service.probeAndHealCandidates(
		ctx, f.run, candidates, []string{f.entry.Name}, newErrorCache(), RunOptions{}, true, confirm,
	)
	if err != nil {
		t.Fatal(err)
	}
	return candidates
}

// serveRadarr serves one Radarr movie per entry file, each a symlink into the entry.
func (f *sweepFixture) serveRadarr(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	var movies []string
	for name := range f.entry.Files {
		link := filepath.Join(dir, name)
		if err := os.Symlink(filepath.Join("/mnt/decypharr", f.entry.Name, name), link); err != nil {
			t.Fatal(err)
		}
		id := len(movies) + 1
		movies = append(movies, fmt.Sprintf(`{"id":%d,"title":%q,"movieFile":{"id":%d,"path":%q}}`, id, name, id, link))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/movie" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, "[%s]", strings.Join(movies, ","))
	}))
	t.Cleanup(server.Close)
	registry := arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	registry.AddOrUpdate(arr.Arr{Name: "radarr", Host: server.URL, Token: "token", Type: arr.Radarr})
	f.service.arrs = registry
}

func (f *sweepFixture) sweep(ctx context.Context, stop *repairStopState) {
	autoRepair := true
	f.service.executeSweep(ctx, &storage.RepairRun{ID: "sweep"}, RunOptions{AutoRepair: &autoRepair}, stop)
}

func (f *sweepFixture) setStoredStatus(t *testing.T, status storage.HealthStatus) {
	t.Helper()
	health, err := f.store.GetEntryHealth(f.entry.Name)
	if err != nil {
		t.Fatal(err)
	}
	health.Status = status
	if err = f.store.SaveEntryHealth(health); err != nil {
		t.Fatal(err)
	}
}

func TestSweepReacquiresOnSecondBrokenProbe(t *testing.T) {
	t.Parallel()
	f := newSweepFixture(t, "video.mkv")
	f.probe(t.Context(), t, true)
	if len(f.reacquirer.requests) != 0 {
		t.Fatalf("first broken probe queued %d reacquisitions", len(f.reacquirer.requests))
	}
	if saved, _ := f.store.GetEntryHealth(f.entry.Name); saved == nil || saved.Status != storage.HealthBroken {
		t.Fatalf("saved health = %#v, want broken", saved)
	}
	f.probe(t.Context(), t, true)
	if len(f.reacquirer.requests) != 1 || f.reacquirer.requests[0].FileID != "id-video.mkv" {
		t.Fatalf("second broken probe requests = %#v", f.reacquirer.requests)
	}
}

func TestScheduledSweepConfirmsBeforeReacquiring(t *testing.T) {
	t.Parallel()
	f := newSweepFixture(t, "a.mkv", "b.mkv")
	f.serveRadarr(t)
	f.sweep(t.Context(), nil)
	if len(f.reacquirer.requests) != 0 {
		t.Fatalf("first sweep queued %#v", f.reacquirer.requests)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.reacquirer.reacquire = func(reacquire.Request) (*reacquire.Job, error) {
		cancel()
		return &reacquire.Job{ID: "job"}, nil
	}
	stop := &repairStopState{}
	stop.set()
	f.sweep(ctx, stop)
	if len(f.reacquirer.requests) != 2 || f.reacquirer.requests[0].FileID == f.reacquirer.requests[1].FileID {
		t.Fatalf("stopped second sweep requests = %#v, want both files once", f.reacquirer.requests)
	}
}

func TestSweepConfirmsAfterCrashRestore(t *testing.T) {
	t.Parallel()
	f := newSweepFixture(t, "video.mkv")
	f.probe(t.Context(), t, true)
	var crashed *storage.EntryHealth
	f.backend.reinsert = func(*storage.Entry) error {
		crashed, _ = f.store.GetEntryHealth(f.entry.Name)
		return errors.New("no replacement")
	}
	f.probe(t.Context(), t, true)
	if crashed == nil || crashed.Status != storage.HealthRepairing {
		t.Fatalf("mid-probe health = %#v, want repairing", crashed)
	}
	if err := f.store.SaveEntryHealth(crashed); err != nil {
		t.Fatal(err)
	}
	f.service.revertStuckHealths()
	f.probe(t.Context(), t, true)
	if len(f.reacquirer.requests) != 2 {
		t.Fatalf("requests = %d, want one per confirming probe", len(f.reacquirer.requests))
	}
}

func TestSweepDoesNotConfirmAnotherReleasesBrokenFile(t *testing.T) {
	t.Parallel()
	f := newSweepFixture(t, "video.mkv")
	if err := f.store.SaveEntryHealth(&storage.EntryHealth{
		EntryName: f.entry.Name, Status: storage.HealthBroken,
		BrokenFiles: []storage.BrokenFile{{FileName: "video.mkv", InfoHash: "another-release"}},
	}); err != nil {
		t.Fatal(err)
	}
	f.probe(t.Context(), t, true)
	if len(f.reacquirer.requests) != 0 {
		t.Fatalf("requests = %#v, want none", f.reacquirer.requests)
	}
}

func TestSweepConfirmationResetsAfterNonBrokenStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []storage.HealthStatus{storage.HealthHealthy, storage.HealthUnknown, storage.HealthRepairing} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			f := newSweepFixture(t, "video.mkv")
			f.probe(t.Context(), t, true)
			f.setStoredStatus(t, status)
			f.probe(t.Context(), t, true)
			if len(f.reacquirer.requests) != 0 {
				t.Fatalf("requests after %s = %#v, want none", status, f.reacquirer.requests)
			}
			f.probe(t.Context(), t, true)
			if len(f.reacquirer.requests) != 1 {
				t.Fatalf("requests = %d, want 1", len(f.reacquirer.requests))
			}
		})
	}
}

func TestRecheckHealsFirstBrokenProbe(t *testing.T) {
	t.Parallel()
	f := newSweepFixture(t, "video.mkv")
	f.probe(t.Context(), t, false)
	if len(f.reacquirer.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(f.reacquirer.requests))
	}
}

func TestRecheckEntryWithFixHealsFirstBrokenProbe(t *testing.T) {
	t.Parallel()
	f := newSweepFixture(t, "video.mkv")
	f.serveRadarr(t)
	if _, err := f.service.RecheckEntry(t.Context(), f.entry.Name, true); err != nil {
		t.Fatal(err)
	}
	f.service.runWG.Wait()
	if len(f.reacquirer.requests) != 1 || f.reacquirer.requests[0].FileID != "id-video.mkv" {
		t.Fatalf("requests = %#v, want one for video.mkv", f.reacquirer.requests)
	}
}

func TestStoppedSweepFinishesOnlyCutHeals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		cut, cutCall  bool
		inline, total int
	}{
		{name: "inline heal complete", inline: 2, total: 2},
		{name: "inline heal cut", cut: true, inline: 1, total: 2},
		{name: "reacquire call cut", cut: true, cutCall: true, inline: 1, total: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSweepFixture(t, "a.mkv", "b.mkv")
			f.probe(t.Context(), t, true)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cut {
				f.reacquirer.reacquire = cutFirstReacquire(f.reacquirer, cancel, tc.cutCall)
			}
			candidates := f.probe(ctx, t, true)
			if inline := len(f.reacquirer.requests); inline != tc.inline {
				t.Fatalf("inline requests = %d, want %d", inline, tc.inline)
			}
			cancel()
			stop := &repairStopState{}
			stop.set()
			f.service.finishCancelledRepairSweep(ctx, f.run, stop, true, "stopped", candidates)
			if len(f.reacquirer.requests) != tc.total || len(fileIDs(f.reacquirer.requests)) != 2 {
				t.Fatalf("requests after finish = %#v, want %d covering both files", f.reacquirer.requests, tc.total)
			}
			if f.run.Stats.Repaired != 2 || f.run.Stats.RepairFailed != 0 {
				t.Fatalf("stats = %#v, want 2 repaired and none failed", f.run.Stats)
			}
		})
	}
}

func fileIDs(requests []reacquire.Request) map[string]bool {
	ids := make(map[string]bool, len(requests))
	for _, request := range requests {
		ids[request.FileID] = true
	}
	return ids
}

// cutFirstReacquire cancels the run during the first reacquire call, failing that call when failCall is set.
func cutFirstReacquire(
	fake *fakeReacquirer,
	cancel context.CancelFunc,
	failCall bool,
) func(reacquire.Request) (*reacquire.Job, error) {
	return func(reacquire.Request) (*reacquire.Job, error) {
		if len(fake.requests) == 1 {
			cancel()
			if failCall {
				return nil, context.Canceled
			}
		}
		return &reacquire.Job{ID: "job"}, nil
	}
}
