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
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// outageClient is one debrid provider whose hosters can be switched off per release or all at once.
type outageClient struct {
	debrid.Client

	mu      sync.Mutex
	down    map[string]bool
	allDown bool
	checks  int
	// onCheck, when set, runs on every check with the number of checks so far.
	onCheck func(checks int)
	// blockCheck makes a check wait until its context ends, as a provider that never answers would.
	blockCheck bool
}

func (c *outageClient) Config() config.Debrid {
	return config.Debrid{Name: "rd", Provider: "realdebrid"}
}
func (c *outageClient) SupportsCheck() bool { return true }

func (c *outageClient) CheckFile(ctx context.Context, infoHash, _ string) error {
	c.mu.Lock()
	block := c.blockCheck
	c.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks++
	if c.onCheck != nil {
		c.onCheck(c.checks)
	}
	if c.allDown || c.down[infoHash] {
		return customerror.ErrHosterUnavailable
	}
	return nil
}

func (c *outageClient) set(allDown bool, hashes ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.allDown = allDown
	c.down = make(map[string]bool, len(hashes))
	for _, hash := range hashes {
		c.down[hash] = true
	}
}

// outageFixture is a library of single-file movies on one debrid provider, each known to a Radarr.
type outageFixture struct {
	store      *storage.Storage
	service    *Service
	reacquirer *fakeReacquirer
	client     *outageClient
	clock      *fakeClock
	entries    []*storage.Entry
	runs       int
	run        *storage.RepairRun
	// deleted are the entries whose Radarr file a reacquisition deleted (see openJobsReacquirer).
	deletedMu sync.Mutex
	deleted   map[string]bool
}

func outageHash(i int) string { return fmt.Sprintf("%040x", i+1) }

func newOutageFixture(t *testing.T, size int, repair config.RepairConfig) *outageFixture {
	t.Helper()
	store := newRepairTestStorage(t)
	f := &outageFixture{store: store, client: &outageClient{down: map[string]bool{}}, clock: newFakeClock()}
	for i := range size {
		name := fmt.Sprintf("release-%03d", i)
		hash := outageHash(i)
		entry := &storage.Entry{
			InfoHash: hash, Name: name, Protocol: config.ProtocolTorrent, ActiveProvider: "rd",
			Files: map[string]*storage.File{"video.mkv": {ID: "id-" + name, Name: "video.mkv", InfoHash: hash}},
			Providers: map[string]*storage.ProviderEntry{
				"rd": {
					Provider: "rd", ID: "rd-" + name, Status: debridTypes.TorrentStatusDownloaded,
					Files: map[string]*storage.ProviderFile{"video.mkv": {Link: "https://rd/" + name}},
				},
			},
		}
		if err := store.AddOrUpdate(entry); err != nil {
			t.Fatal(err)
		}
		f.entries = append(f.entries, entry)
	}
	f.reacquirer = &fakeReacquirer{reacquire: func(reacquire.Request) (*reacquire.Job, error) {
		return &reacquire.Job{ID: "job"}, nil
	}}
	backend := &removedProviderBackend{
		reinsert: func(*storage.Entry) error { return fmt.Errorf("no replacement") },
		client:   f.client,
	}
	f.service = New(Dependencies{
		Storage: store, Backend: backend, Reacquirer: f.reacquirer,
		Config: config.NewStore(&config.Config{Repair: repair}), Logger: zerolog.Nop(),
	})
	f.service.now = f.clock.Now
	f.serveRadarr(t)
	return f
}

// serveRadarr serves one Radarr movie per entry, each a symlink into the entry.
func (f *outageFixture) serveRadarr(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	links := make([]string, 0, len(f.entries))
	for _, entry := range f.entries {
		link := filepath.Join(dir, entry.Name+".mkv")
		if err := os.Symlink(filepath.Join("/mnt/decypharr", entry.Name, "video.mkv"), link); err != nil {
			t.Fatal(err)
		}
		links = append(links, link)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/movie" {
			http.NotFound(w, r)
			return
		}
		f.deletedMu.Lock()
		defer f.deletedMu.Unlock()
		movies := make([]string, 0, len(f.entries))
		for i, entry := range f.entries {
			if f.deleted[entry.Name] {
				movies = append(movies, fmt.Sprintf(`{"id":%d,"title":%q}`, i+1, entry.Name))
				continue
			}
			movies = append(movies, fmt.Sprintf(`{"id":%d,"title":%q,"movieFile":{"id":%d,"path":%q}}`,
				i+1, entry.Name, i+1, links[i]))
		}
		_, _ = fmt.Fprintf(w, "[%s]", strings.Join(movies, ","))
	}))
	t.Cleanup(server.Close)
	registry := arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	registry.AddOrUpdate(arr.Arr{Name: "radarr", Host: server.URL, Token: "token", Type: arr.Radarr})
	f.service.arrs = registry
}

// sweep runs one scheduled sweep a day after the previous one.
func (f *outageFixture) sweep(ctx context.Context, stop *repairStopState) *storage.RepairRun {
	f.runs++
	f.clock.Advance(25 * time.Hour)
	f.run = &storage.RepairRun{ID: fmt.Sprintf("sweep-%d", f.runs), Trigger: storage.RepairTriggerScheduled}
	autoRepair := true
	f.service.executeSweep(ctx, f.run, RunOptions{AutoRepair: &autoRepair}, stop)
	return f.run
}

func (f *outageFixture) health(t *testing.T, i int) *storage.EntryHealth {
	t.Helper()
	h, err := f.store.GetEntryHealth(f.entries[i].Name)
	if err != nil {
		t.Fatalf("health of entry %d: %v", i, err)
	}
	return h
}

func (f *outageFixture) requests() int { return len(f.reacquirer.requests) }

// downRange switches off the hosters of entries [from, to).
func (f *outageFixture) downRange(from, to int) {
	var hashes []string
	for i := from; i < to; i++ {
		hashes = append(hashes, outageHash(i))
	}
	f.client.set(false, hashes...)
}

// markUpDirty makes the entries the outage left up, 80 onward, due for the next sweep.
func (f *outageFixture) markUpDirty(t *testing.T) {
	t.Helper()
	for i := 80; i < len(f.entries); i++ {
		if err := f.store.MarkEntryDirty(f.entries[i].Name, config.ProtocolTorrent, "entry_item_changed"); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *outageFixture) countStatus(t *testing.T, status storage.HealthStatus) int {
	t.Helper()
	n := 0
	for i := range f.entries {
		if f.health(t, i).Status == status {
			n++
		}
	}
	return n
}

func rdHoster() Bucket { return Bucket{Provider: "rd", Class: ClassHosterLink} }

// The incident of 2026-10-12: a hoster problem makes most of one provider look broken for several sweeps.
func TestOutageAcrossSweepsHealsNothingAndTripsTheBucket(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.downRange(0, 80)

	f.sweep(t.Context(), nil)
	if f.requests() != 0 {
		t.Fatalf("the first observation healed %d entries", f.requests())
	}
	f.sweep(t.Context(), nil) // second observation: 80 of 100 confirmed
	f.sweep(t.Context(), nil) // the outage is still on
	f.sweep(t.Context(), nil)
	if f.requests() != 0 {
		t.Fatalf("an outage lasting four sweeps healed %d entries, want none", f.requests())
	}
	if got := f.countStatus(t, storage.HealthHeld); got != 80 {
		t.Fatalf("held entries = %d, want 80", got)
	}
	if got := f.countStatus(t, storage.HealthBroken); got != 0 {
		t.Fatalf("%d entries are broken: a held entry must never be stored as broken", got)
	}
	status := f.service.BreakerStatus()
	if len(status) != 1 || status[0].Provider != "rd" || status[0].Class != ClassHosterLink ||
		status[0].State != "tripped" {
		t.Fatalf("breaker status = %+v, want rd/hoster_link tripped", status)
	}
	held := f.health(t, 0)
	if held.HeldBucket != rdHoster().String() || len(held.BrokenFiles) != 1 ||
		held.BrokenFiles[0].Reason != "hoster_unavailable" {
		t.Fatalf("held row = %#v", held)
	}
}

func TestNormalSweepWithOneBrokenEntryHealsIt(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.downRange(7, 8)

	f.sweep(t.Context(), nil)
	if f.requests() != 0 {
		t.Fatalf("a first observation healed %d entries", f.requests())
	}
	f.sweep(t.Context(), nil)
	if f.requests() != 1 || f.reacquirer.requests[0].FileID != "id-release-007" {
		t.Fatalf("requests = %#v, want exactly the one broken entry", f.reacquirer.requests)
	}
	if f.run.Stats.Repaired != 1 || f.run.Stats.Held != 0 {
		t.Fatalf("stats = %#v", f.run.Stats)
	}
	if len(f.service.BreakerStatus()) != 0 {
		t.Fatalf("a normal sweep opened a bucket: %+v", f.service.BreakerStatus())
	}
}

func TestSweepHealsAreCappedPerBucketButNotHeld(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{BreakerMinConfirmed: 100})
	f.downRange(0, 40)

	f.sweep(t.Context(), nil)
	f.sweep(t.Context(), nil)
	if f.requests() != 25 {
		t.Fatalf("heals = %d, want the bucket cap of 25", f.requests())
	}
	if f.run.Stats.Held != 0 {
		t.Fatalf("a cap is not a hold: stats = %#v", f.run.Stats)
	}
	if got := f.countStatus(t, storage.HealthBroken); got != 40 {
		t.Fatalf("broken entries = %d, want all 40 left confirmed for later (25 healed ones stay until replaced)", got)
	}
}

func TestCanaryCatchesAnOutageUnderTheThreshold(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.sweep(t.Context(), nil) // everything is healthy and was good a day ago
	// The hosters go down for the whole provider, but only three entries come due.
	f.client.set(true)
	for i := range 3 {
		if err := f.store.MarkEntryDirty(f.entries[i].Name, config.ProtocolTorrent, "entry_item_changed"); err != nil {
			t.Fatal(err)
		}
	}
	f.sweep(t.Context(), nil)
	f.sweep(t.Context(), nil)
	if f.requests() != 0 {
		t.Fatalf("heals = %d, want none while recently good entries fail the same way", f.requests())
	}
	if got := f.countStatus(t, storage.HealthHeld); got != 3 {
		t.Fatalf("held = %d, want the 3 confirmed entries", got)
	}
	if status := f.service.BreakerStatus(); len(status) != 1 || status[0].Reason != "canary_failed" {
		t.Fatalf("breaker status = %+v, want a canary trip", status)
	}
}

// SM12-20, live 2026-11-09..12: 134 held usenet entries, real losses, re-tripped their bucket every sweep. Held
// entries that are still broken are the failure that tripped the bucket, not new evidence: a clean sample of the
// entries that are not held releases it, and the held ones drain through the caps.
func TestHeldEntriesStillBrokenDoNotKeepTheBucketTripped(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.downRange(0, 80)
	f.sweep(t.Context(), nil)
	f.sweep(t.Context(), nil)
	if f.requests() != 0 || f.countStatus(t, storage.HealthHeld) != 80 {
		t.Fatalf("setup: requests %d held %d", f.requests(), f.countStatus(t, storage.HealthHeld))
	}

	// The 80 stay broken; the other 20 come due and probe clean.
	f.markUpDirty(t)
	f.sweep(t.Context(), nil)
	if f.requests() != 25 {
		t.Fatalf("heals = %d, want the bucket released and the held entries drained up to the cap of 25", f.requests())
	}
	if got := f.countStatus(t, storage.HealthHeld); got != 55 {
		t.Fatalf("still held = %d, want the 55 the cap left for later sweeps of the drain", got)
	}
}

// openJobsReacquirer reports every requested reacquisition as a queued job created when it was requested, as the
// reacquire journal does while the Arr has not replaced the file yet, and deletes the entry's Radarr file as a
// reacquisition does, so the entry is no candidate of a later sweep.
type openJobsReacquirer struct {
	*fakeReacquirer

	f  *outageFixture
	at *[]time.Time
}

func newOpenJobsReacquirer(f *outageFixture) openJobsReacquirer {
	return openJobsReacquirer{fakeReacquirer: f.reacquirer, f: f, at: new([]time.Time)}
}

func (j openJobsReacquirer) Reacquire(request reacquire.Request) (*reacquire.Job, error) {
	j.f.deletedMu.Lock()
	if j.f.deleted == nil {
		j.f.deleted = map[string]bool{}
	}
	j.f.deleted[strings.TrimPrefix(request.FileID, "id-")] = true
	j.f.deletedMu.Unlock()
	*j.at = append(*j.at, j.f.clock.Now())
	return j.fakeReacquirer.Reacquire(request)
}

func (j openJobsReacquirer) Jobs() []reacquire.Job {
	jobs := make([]reacquire.Job, 0, len(j.requests))
	for i, request := range j.requests {
		jobs = append(jobs, reacquire.Job{
			Status: reacquire.StatusQueued, EntryID: request.EntryID, CreatedAt: (*j.at)[i],
		})
	}
	return jobs
}

// Item 54: after a release the cap drains the held entries over several sweeps. The ones the cap left wait held,
// the healed ones (their reacquisitions still open) are no new evidence, so the bucket never re-trips, and each
// sweep heals entries not healed before until the drain ends and the bucket closes.
func TestReleaseDrainsHeldEntriesAcrossSweepsWithoutRetripping(t *testing.T) {
	t.Parallel()
	t.Run("a clean sample every sweep", func(t *testing.T) {
		t.Parallel()
		assertDrains(t, true)
	})
	// The healthy entries are not due again for the recheck interval, so later drain sweeps see only held rows.
	t.Run("no sample after the release", func(t *testing.T) {
		t.Parallel()
		assertDrains(t, false)
	})
}

func assertDrains(t *testing.T, sampleEverySweep bool) {
	t.Helper()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.service.reacquirer = newOpenJobsReacquirer(f)
	f.downRange(0, 80)
	f.sweep(t.Context(), nil)
	f.sweep(t.Context(), nil)

	for sweep, want := range []int{25, 50, 75, 80, 80} {
		if sweep == 0 || sampleEverySweep {
			f.markUpDirty(t)
		}
		f.sweep(t.Context(), nil)
		if got := f.requests(); got != want {
			t.Fatalf("drain sweep %d: heals = %d, want %d (25 new per sweep)", sweep, got, want)
		}
		assertNotTripped(t, f, fmt.Sprintf("drain sweep %d", sweep))
	}
	healed := map[string]bool{}
	for _, request := range f.reacquirer.requests {
		if healed[request.FileID] {
			t.Fatalf("%s healed twice", request.FileID)
		}
		healed[request.FileID] = true
	}
	if got := f.countStatus(t, storage.HealthHeld); got != 0 {
		t.Fatalf("still held = %d after the drain", got)
	}
	if status := f.service.BreakerStatus(); len(status) != 0 {
		t.Fatalf("the bucket did not close after the drain: %+v", status)
	}
}

// The counterpart: when entries that are not held break too, the bucket stays tripped and nothing heals.
func TestHeldBucketStaysTrippedWhenTheSampleIsDirty(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.downRange(0, 80)
	f.sweep(t.Context(), nil)
	f.sweep(t.Context(), nil)

	f.downRange(0, 95)
	f.markUpDirty(t)
	f.sweep(t.Context(), nil) // first sighting of 80..94
	f.sweep(t.Context(), nil) // 15 non-held entries confirmed
	if f.requests() != 0 {
		t.Fatalf("heals = %d, want none while the non-held sample is broken", f.requests())
	}
	if got := f.countStatus(t, storage.HealthHeld); got != 95 {
		t.Fatalf("held = %d, want all 95 confirmed entries", got)
	}
	if status := f.service.BreakerStatus(); len(status) != 1 || status[0].State != "tripped" {
		t.Fatalf("breaker status = %+v, want tripped", status)
	}
}

func TestReleaseReprobesHeldEntriesBeforeHealingTheRest(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.downRange(0, 80)
	f.sweep(t.Context(), nil)
	f.sweep(t.Context(), nil)
	if f.requests() != 0 || f.countStatus(t, storage.HealthHeld) != 80 {
		t.Fatalf("setup: requests %d held %d", f.requests(), f.countStatus(t, storage.HealthHeld))
	}

	// The outage ends. Five releases are genuinely broken.
	f.downRange(0, 5)
	f.sweep(t.Context(), nil)
	if f.requests() != 5 {
		t.Fatalf("heals after release = %d, want only the 5 genuinely broken", f.requests())
	}
	healed := map[string]bool{}
	for _, request := range f.reacquirer.requests {
		healed[request.FileID] = true
	}
	for i := range 5 {
		if !healed["id-"+f.entries[i].Name] {
			t.Errorf("entry %d was not healed", i)
		}
	}
	if got := f.countStatus(t, storage.HealthHealthy); got != 95 {
		t.Fatalf("healthy entries = %d, want the 75 re-probed and the 20 never broken", got)
	}
	if got := f.countStatus(t, storage.HealthHeld); got != 0 {
		t.Fatalf("still held = %d", got)
	}
	if len(f.service.BreakerStatus()) != 0 {
		t.Fatalf("the bucket did not close after draining: %+v", f.service.BreakerStatus())
	}
}

// startDrain trips the bucket on 80 of 100 entries and runs the first drain sweep: 25 healed, 55 held.
func startDrain(t *testing.T) *outageFixture {
	t.Helper()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.service.reacquirer = newOpenJobsReacquirer(f)
	f.downRange(0, 80)
	f.sweep(t.Context(), nil)
	f.sweep(t.Context(), nil)
	f.markUpDirty(t)
	f.sweep(t.Context(), nil)
	if f.requests() != 25 || f.countStatus(t, storage.HealthHeld) != 55 {
		t.Fatalf("setup: heals %d held %d", f.requests(), f.countStatus(t, storage.HealthHeld))
	}
	return f
}

func assertNotTripped(t *testing.T, f *outageFixture, when string) {
	t.Helper()
	for _, status := range f.service.BreakerStatus() {
		if status.State == string(breakerTripped) {
			t.Fatalf("%s: the bucket re-tripped: %+v", when, status)
		}
	}
}

// Item 54 review: a recheck of one held entry mid-drain must not close the drain while other entries stay held.
func TestRecheckDuringTheDrainDoesNotEndIt(t *testing.T) {
	t.Parallel()
	f := startDrain(t)
	f.clock.Advance(2 * time.Hour)
	if _, err := f.service.RecheckEntry(t.Context(), f.entries[79].Name, RecheckOptions{Fix: true}); err != nil {
		t.Fatal(err)
	}
	f.service.runWG.Wait()
	if status := f.service.BreakerStatus(); len(status) != 1 || status[0].State != string(breakerReleasing) {
		t.Fatalf("after the recheck: %+v, want the bucket still releasing", status)
	}
	f.markUpDirty(t)
	f.sweep(t.Context(), nil)
	assertNotTripped(t, f, "sweep after the recheck")
	if got := f.requests(); got < 50 {
		t.Fatalf("heals = %d, want the drain to go on", got)
	}
}

// Item 54 review: sweeps re-sighting held entries must not fill the rolling window, so a heal admitted outside a
// sweep mid-drain does not re-trip the bucket.
func TestAdmitDuringTheDrainDoesNotRetrip(t *testing.T) {
	t.Parallel()
	f := startDrain(t)
	f.clock.Advance(30 * time.Minute) // inside the window the drain sweep's sightings are in
	err := f.service.Admit(t.Context(), HealRequest{Provider: "rd", Class: ClassHosterLink, Subject: "other"})
	if err != nil && !errors.Is(err, ErrHealCapped) {
		t.Fatalf("admit mid-drain: %v, want admitted or capped (the drain used the bucket's cap), never held", err)
	}
	assertNotTripped(t, f, "admit")
}
