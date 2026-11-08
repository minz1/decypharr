package repair

import (
	"context"
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
	"github.com/sirrobot01/decypharr/internal/task"
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
	// entered, when set, receives a value each time a blocked check starts waiting.
	entered chan struct{}
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
		if c.entered != nil {
			c.entered <- struct{}{}
		}
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
	exits      exitWatch
	g          *task.Group
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
	f.g = testGroup(t)
	f.service = New(f.g, Dependencies{
		Storage: store, Backend: backend, Reacquirer: f.reacquirer,
		Config: config.NewStore(&config.Config{Repair: repair}), Logger: zerolog.Nop(),
	})
	f.service.now = f.clock.Now
	f.exits = f.service.watchExits()
	f.serveRadarr(t)
	return f
}

// serveRadarr serves one Radarr movie per entry, each a symlink into the entry.
func (f *outageFixture) serveRadarr(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	movies := make([]string, 0, len(f.entries))
	for i, entry := range f.entries {
		link := filepath.Join(dir, entry.Name+".mkv")
		if err := os.Symlink(filepath.Join("/mnt/decypharr", entry.Name, "video.mkv"), link); err != nil {
			t.Fatal(err)
		}
		movies = append(movies, fmt.Sprintf(`{"id":%d,"title":%q,"movieFile":{"id":%d,"path":%q}}`,
			i+1, entry.Name, i+1, link))
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
