package manager

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const pruneGrace = 168 * time.Hour

type pruneHarness struct {
	m      *Manager
	root   string
	mount  string
	now    time.Time
	logs   *bytes.Buffer
	fake   *fakeArrRecovery
	handed []PruneCandidate
	dbDir  string
}

func newPruneHarness(t *testing.T, mode config.PruneMode) *pruneHarness {
	t.Helper()
	dbDir := filepath.Join(t.TempDir(), "db")
	store, err := storage.NewStorage(dbDir, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := &pruneHarness{
		dbDir: dbDir,
		root:  t.TempDir(),
		mount: filepath.Join(t.TempDir(), "mnt"),
		now:   time.Unix(1_800_000_000, 0),
		logs:  &bytes.Buffer{},
		fake:  &fakeArrRecovery{},
	}
	cfg := config.NewStore(&config.Config{
		Mount: config.Mount{MountPath: h.mount},
		Prune: config.Prune{
			Enabled: true, Mode: mode, Schedule: "1h", Grace: "168h", MaxPerRun: 100,
			Roots: []string{h.root},
		},
	})
	h.m = &Manager{
		clients:  xsync.NewMap[string, debrid.Client](),
		storage:  store,
		queue:    newQueue(store, "", nil, zerolog.Nop()),
		store:    cfg,
		logger:   zerolog.New(h.logs),
		pruneNow: func() time.Time { return h.now },
		pruneEntriesF: func(_ context.Context, eligible []PruneCandidate) (int, int, int64) {
			h.handed = slices.Clone(eligible)
			return 0, 0, 0
		},
	}
	h.m.SetArrRecovery(h.fake)
	t.Cleanup(func() { _ = h.m.storage.Close() })
	return h
}

func (h *pruneHarness) corruptLedger(t *testing.T, key string) {
	t.Helper()
	if err := h.m.storage.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := appendstore.Open(filepath.Join(h.dbDir, "prune.db"), appendstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = raw.Put(key, []byte{1, 2, 3}, nil); err != nil {
		t.Fatal(err)
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewStorage(h.dbDir, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h.m.storage = store
	h.m.queue = newQueue(store, "", nil, zerolog.Nop())
}

func (h *pruneHarness) setPrune(t *testing.T, edit func(*config.Prune)) {
	t.Helper()
	next := *h.m.store.Get()
	edit(&next.Prune)
	h.m.store = config.NewStore(&next)
}

func (h *pruneHarness) addEntry(t *testing.T, hash string, edit func(*storage.Entry)) *storage.Entry {
	t.Helper()
	e := &storage.Entry{
		InfoHash:       hash,
		Name:           "Show." + hash,
		Size:           100,
		State:          storage.EntryStatePausedUP,
		IsComplete:     true,
		ActiveProvider: "torbox",
		Providers:      map[string]*storage.ProviderEntry{"torbox": {Provider: "torbox", ID: "id-" + hash}},
		Files: map[string]*storage.File{
			"a.mkv": {Name: "a.mkv", Size: 100, InfoHash: hash, AddedOn: h.now},
		},
	}
	if edit != nil {
		edit(e)
	}
	if err := h.m.storage.AddOrUpdate(e); err != nil {
		t.Fatal(err)
	}
	return e
}

func (h *pruneHarness) reference(t *testing.T, e *storage.Entry) {
	t.Helper()
	target := filepath.Join(h.mount, "__all__", e.Name, "a.mkv")
	linkAt(t, h.root, "link-"+e.InfoHash, target)
}

func (h *pruneHarness) unreference(t *testing.T, e *storage.Entry) {
	t.Helper()
	if err := os.Remove(filepath.Join(h.root, "link-"+e.InfoHash)); err != nil {
		t.Fatal(err)
	}
}

func (h *pruneHarness) run(t *testing.T) PruneReport {
	t.Helper()
	r, err := h.m.RunPrune(context.Background())
	if err != nil {
		t.Fatalf("RunPrune: %v (report %+v)", err, r)
	}
	return r
}

func (h *pruneHarness) age(t *testing.T, d time.Duration) {
	t.Helper()
	for d > 24*time.Hour {
		h.now = h.now.Add(24 * time.Hour)
		d -= 24 * time.Hour
		h.run(t)
	}
	h.now = h.now.Add(d)
}

func (h *pruneHarness) edit(t *testing.T, fn func(*config.Config)) {
	t.Helper()
	next := *h.m.store.Get()
	fn(&next)
	h.m.store = config.NewStore(&next)
}

func (h *pruneHarness) ledger(t *testing.T) map[string]time.Time {
	t.Helper()
	l, err := h.m.storage.UnreferencedSince()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func candidate(t *testing.T, r PruneReport, hash string) PruneCandidate {
	t.Helper()
	for _, c := range r.Candidates {
		if c.InfoHash == hash {
			return c
		}
	}
	t.Fatalf("no candidate %s in %+v", hash, r.Candidates)
	return PruneCandidate{}
}

func eligibleHashes(h *pruneHarness, r PruneReport) []string {
	var out []string
	for _, c := range r.Candidates {
		if c.eligible(h.now) {
			out = append(out, c.InfoHash)
		}
	}
	return out
}

func TestPruneFirstScanStartsClock(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	kept := h.addEntry(t, "aaaa", nil)
	h.reference(t, kept)
	h.addEntry(t, "bbbb", nil)

	r := h.run(t)

	if r.Aborted != "" || r.NewlyUnreferenced != 1 || r.Referenced != 1 || r.Links != 1 {
		t.Fatalf("report %+v", r)
	}
	c := candidate(t, r, "bbbb")
	if !c.UnreferencedSince.Equal(h.now) || !c.EligibleAt.Equal(h.now.Add(pruneGrace)) {
		t.Fatalf("candidate %+v", c)
	}
	if got := h.ledger(t); len(got) != 1 || !got["bbbb"].Equal(h.now) {
		t.Fatalf("ledger %v", got)
	}
	if len(eligibleHashes(h, r)) != 0 || len(h.handed) != 0 {
		t.Fatalf("nothing may be eligible on the first scan: %v %v", r.Candidates, h.handed)
	}
}

func TestPruneEligibleAfterGrace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		wait time.Duration
		want bool
	}{
		{"just under", pruneGrace - time.Minute, false},
		{"exactly", pruneGrace, true},
		{"over", pruneGrace + time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newPruneHarness(t, config.PruneModeDelete)
			keep := h.addEntry(t, "aaaa", nil)
			h.reference(t, keep)
			h.addEntry(t, "bbbb", nil)
			h.run(t)
			h.handed = nil

			h.age(t, tc.wait)
			r := h.run(t)

			if got := candidate(t, r, "bbbb").eligible(h.now); got != tc.want {
				t.Fatalf("eligible=%v want %v", got, tc.want)
			}
			if (len(h.handed) == 1) != tc.want {
				t.Fatalf("handed %v want eligible=%v", h.handed, tc.want)
			}
		})
	}
}

func TestPruneReReferencedClearsAndRestarts(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeReport)
	for _, hash := range []string{"a01", "a02", "a03", "a04", "a05", "a06", "a07", "a08", "a09", "a10", "a11", "a12"} {
		h.reference(t, h.addEntry(t, hash, nil))
	}
	flip := h.addEntry(t, "bbbb", nil)
	first := h.now
	h.run(t)

	h.now = first.Add(5 * 24 * time.Hour)
	h.reference(t, flip)
	h.run(t)
	if _, ok := h.ledger(t)["bbbb"]; ok {
		t.Fatal("referenced entry kept its ledger key")
	}

	h.now = first.Add(6 * 24 * time.Hour)
	h.unreference(t, flip)
	r := h.run(t)
	c := candidate(t, r, "bbbb")
	if !c.UnreferencedSince.Equal(h.now) || c.eligible(h.now) {
		t.Fatalf("clock did not restart: %+v", c)
	}
}

func TestPruneExclusions(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	keep := h.addEntry(t, "aaaa", nil)
	h.reference(t, keep)
	h.addEntry(t, "man1", func(e *storage.Entry) { e.Manual = true })
	h.addEntry(t, "que1", nil)
	h.addEntry(t, "inc1", func(e *storage.Entry) { e.IsComplete = false })
	h.addEntry(t, "dow1", func(e *storage.Entry) { e.State = storage.EntryStateDownloading })
	h.addEntry(t, "rea1", nil)
	h.addEntry(t, "rea2", nil)
	h.addEntry(t, "free", nil)
	if err := h.m.queue.Add(&storage.Entry{InfoHash: "que1", Name: "q"}); err != nil {
		t.Fatal(err)
	}
	h.fake.jobs = []reacquire.Job{
		{EntryID: "rea1", Status: reacquire.StatusSearching},
		{EntryID: "rea2", Status: reacquire.StatusFailed},
	}
	h.run(t)
	h.age(t, pruneGrace)
	r := h.run(t)

	want := map[string]string{
		"man1": "manual", "que1": "queued", "inc1": "incomplete", "dow1": "incomplete",
		"rea1": "reacquire", "rea2": "", "free": "",
	}
	for hash, reason := range want {
		if got := candidate(t, r, hash).Excluded; got != reason {
			t.Errorf("%s excluded=%q want %q", hash, got, reason)
		}
	}
	got := eligibleHashes(h, r)
	slices.Sort(got)
	if !slices.Equal(got, []string{"free", "rea2"}) {
		t.Fatalf("eligible %v", got)
	}
	if len(h.handed) != 2 {
		t.Fatalf("handed %v", h.handed)
	}
	handed := []string{h.handed[0].InfoHash, h.handed[1].InfoHash}
	slices.Sort(handed)
	if !slices.Equal(handed, []string{"free", "rea2"}) {
		t.Fatalf("handed %v", h.handed)
	}
}

func setupAbortHarness(t *testing.T, refs int) (*pruneHarness, []*storage.Entry) {
	t.Helper()
	h := newPruneHarness(t, config.PruneModeDelete)
	var es []*storage.Entry
	for i := range refs {
		e := h.addEntry(t, fmt.Sprintf("r%03d", i), nil)
		h.reference(t, e)
		es = append(es, e)
	}
	h.addEntry(t, "orph", nil)
	return h, es
}

func assertAborted(t *testing.T, h *pruneHarness, r PruneReport) {
	t.Helper()
	if r.Aborted == "" {
		t.Fatalf("not aborted: %+v", r)
	}
	if got := h.ledger(t); len(got) != 0 {
		t.Fatalf("ledger written: %v", got)
	}
	if len(h.handed) != 0 || r.Deleted != 0 {
		t.Fatalf("deleted despite abort: %v", h.handed)
	}
	out := h.logs.String()
	if strings.Count(out, `"message":"prune aborted"`) != 1 ||
		!strings.Contains(out, `"reason":`) || !strings.Contains(out, `"level":"warn"`) {
		t.Fatalf("abort log: %s", out)
	}
}

func TestPruneAbortsWithoutLedgerWrites(t *testing.T) {
	t.Parallel()
	t.Run("unreadable root", func(t *testing.T) {
		t.Parallel()
		h, _ := setupAbortHarness(t, 2)
		h.setPrune(t, func(p *config.Prune) { p.Roots = []string{filepath.Join(h.root, "missing")} })
		r, _ := h.m.RunPrune(context.Background())
		assertAborted(t, h, r)
	})
	t.Run("zero references", func(t *testing.T) {
		t.Parallel()
		h, es := setupAbortHarness(t, 2)
		for _, e := range es {
			h.unreference(t, e)
		}
		r, _ := h.m.RunPrune(context.Background())
		assertAborted(t, h, r)
		if _, ok, _ := h.m.storage.LastReferencedCount(); ok {
			t.Fatal("last count written")
		}
	})
	t.Run("below ninety percent", func(t *testing.T) {
		t.Parallel()
		h, _ := setupAbortHarness(t, 89)
		if err := h.m.storage.SetLastReferencedCount(100); err != nil {
			t.Fatal(err)
		}
		r, _ := h.m.RunPrune(context.Background())
		assertAborted(t, h, r)
		if n, _, _ := h.m.storage.LastReferencedCount(); n != 100 {
			t.Fatalf("last count changed to %d", n)
		}
	})
	t.Run("exactly ninety percent runs", func(t *testing.T) {
		t.Parallel()
		h, _ := setupAbortHarness(t, 90)
		if err := h.m.storage.SetLastReferencedCount(100); err != nil {
			t.Fatal(err)
		}
		r := h.run(t)
		if r.Aborted != "" || len(r.Candidates) != 1 {
			t.Fatalf("report %+v", r)
		}
		if n, _, _ := h.m.storage.LastReferencedCount(); n != 90 {
			t.Fatalf("last count %d", n)
		}
	})
}

func TestPruneAbortsOnLedgerReadError(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	keep := h.addEntry(t, "aaaa", nil)
	h.reference(t, keep)
	h.addEntry(t, "bbbb", nil)
	h.run(t)
	h.age(t, pruneGrace)
	h.handed = nil
	h.corruptLedger(t, "bbbb")

	r, err := h.m.RunPrune(context.Background())

	if err == nil || r.Aborted == "" || len(h.handed) != 0 || r.Deleted != 0 {
		t.Fatalf("err=%v report=%+v handed=%v", err, r, h.handed)
	}
	if !strings.Contains(h.logs.String(), `"message":"prune aborted"`) {
		t.Fatalf("no abort log: %s", h.logs.String())
	}
}

func TestPruneReportModeDeletesNothing(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeReport)
	keep := h.addEntry(t, "aaaa", nil)
	h.reference(t, keep)
	h.addEntry(t, "bbbb", nil)
	h.run(t)
	h.age(t, pruneGrace)

	r := h.run(t)

	if len(eligibleHashes(h, r)) != 1 || r.Deleted != 0 || len(h.handed) != 0 {
		t.Fatalf("report %+v handed %v", r, h.handed)
	}
	if _, err := h.m.storage.Get("bbbb"); err != nil {
		t.Fatalf("entry gone: %v", err)
	}
}

func TestPruneDropsLedgerKeysOfVanishedEntries(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeReport)
	keep := h.addEntry(t, "aaaa", nil)
	h.reference(t, keep)
	h.addEntry(t, "bbbb", nil)
	h.run(t)
	if err := h.m.storage.Delete("bbbb"); err != nil {
		t.Fatal(err)
	}

	r := h.run(t)

	if len(h.ledger(t)) != 0 || len(r.Candidates) != 0 {
		t.Fatalf("ledger %v candidates %v", h.ledger(t), r.Candidates)
	}
}

func TestPruneReadsModePerRun(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeReport)
	keep := h.addEntry(t, "aaaa", nil)
	h.reference(t, keep)
	h.addEntry(t, "bbbb", nil)
	h.run(t)
	h.age(t, pruneGrace)

	r := h.run(t)
	if r.Mode != config.PruneModeReport || len(h.handed) != 0 {
		t.Fatalf("report mode: %+v handed %v", r, h.handed)
	}

	h.setPrune(t, func(p *config.Prune) { p.Mode = config.PruneModeDelete })
	r = h.run(t)
	if r.Mode != config.PruneModeDelete || len(h.handed) != 1 || h.handed[0].InfoHash != "bbbb" {
		t.Fatalf("delete mode: %+v handed %v", r, h.handed)
	}
}

func TestPruneDisabledIsRejected(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.reference(t, h.addEntry(t, "aaaa", nil))
	h.addEntry(t, "bbbb", nil)
	h.setPrune(t, func(p *config.Prune) { p.Enabled = false })

	_, err := h.m.RunPrune(context.Background())

	if err == nil || len(h.ledger(t)) != 0 || len(h.handed) != 0 {
		t.Fatalf("err=%v ledger=%v", err, h.ledger(t))
	}
	if _, ok, _ := h.m.storage.LastReferencedCount(); ok {
		t.Fatal("touched ledger")
	}
}

func TestPruneMaxPerRunOrdering(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	keep := h.addEntry(t, "aaaa", nil)
	h.reference(t, keep)
	for _, hash := range []string{"e5", "e4", "e3", "e2", "e1"} {
		h.addEntry(t, hash, nil)
	}
	stamp := func(hash string, ago time.Duration) {
		if err := h.m.storage.SetUnreferencedSince(hash, h.now.Add(-ago)); err != nil {
			t.Fatal(err)
		}
	}
	stamp("e1", 10*24*time.Hour)
	stamp("e2", 10*24*time.Hour)
	stamp("e3", 9*24*time.Hour)
	stamp("e4", 8*24*time.Hour)
	stamp("e5", 7*24*time.Hour+time.Hour)
	if err := h.m.storage.SetLastReferencedCount(1); err != nil {
		t.Fatal(err)
	}
	if err := h.m.storage.SetLastScan(h.now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	h.setPrune(t, func(p *config.Prune) { p.MaxPerRun = 2 })

	r := h.run(t)

	if len(eligibleHashes(h, r)) != 5 {
		t.Fatalf("want 5 eligible: %+v", r.Candidates)
	}
	if len(h.handed) != 2 || h.handed[0].InfoHash != "e1" || h.handed[1].InfoHash != "e2" {
		t.Fatalf("handed %v", h.handed)
	}
}

func TestPruneLastReportIsACopy(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeReport)
	keep := h.addEntry(t, "aaaa", nil)
	h.reference(t, keep)
	h.addEntry(t, "bbbb", nil)
	h.run(t)

	got := h.m.LastPruneReport()
	got.Candidates[0].InfoHash = "mutated"

	if h.m.LastPruneReport().Candidates[0].InfoHash != "bbbb" {
		t.Fatal("LastPruneReport shares its slice")
	}
}

func (h *pruneHarness) seedStamped(t *testing.T, scanAgo time.Duration) {
	t.Helper()
	if err := h.m.storage.SetUnreferencedSince("bbbb", h.now.Add(-10*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if scanAgo > 0 {
		if err := h.m.storage.SetLastScan(h.now.Add(-scanAgo)); err != nil {
			t.Fatal(err)
		}
	}
}

func scanGapHarness(t *testing.T) *pruneHarness {
	t.Helper()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.reference(t, h.addEntry(t, "aaaa", nil))
	h.addEntry(t, "bbbb", nil)
	return h
}

func TestPruneScanGapRestartsClocks(t *testing.T) {
	t.Parallel()
	h := scanGapHarness(t)
	h.seedStamped(t, 72*time.Hour)

	r := h.run(t)

	c := candidate(t, r, "bbbb")
	if !c.UnreferencedSince.Equal(h.now) || c.eligible(h.now) || r.NewlyUnreferenced != 1 || len(h.handed) != 0 {
		t.Fatalf("candidate %+v report %+v", c, r)
	}
	if !h.ledger(t)["bbbb"].Equal(h.now) {
		t.Fatalf("ledger %v", h.ledger(t))
	}
	if !strings.Contains(h.logs.String(), `"message":"prune clocks restarted"`) {
		t.Fatalf("no restart log: %s", h.logs.String())
	}
	if ls, ok, _ := h.m.storage.LastScan(); !ok || !ls.Equal(h.now) {
		t.Fatalf("last scan %v %v", ls, ok)
	}
}

func TestPruneShortScanGapKeepsStamps(t *testing.T) {
	t.Parallel()
	h := scanGapHarness(t)
	h.seedStamped(t, 47*time.Hour)

	r := h.run(t)

	if c := candidate(t, r, "bbbb"); !c.eligible(h.now) || r.NewlyUnreferenced != 0 || len(h.handed) != 1 {
		t.Fatalf("candidate %+v report %+v", c, r)
	}
}

func TestPruneMissingLastScanWithLedgerRestarts(t *testing.T) {
	t.Parallel()
	h := scanGapHarness(t)
	h.seedStamped(t, 0)

	r := h.run(t)

	if c := candidate(t, r, "bbbb"); !c.UnreferencedSince.Equal(h.now) || c.eligible(h.now) {
		t.Fatalf("candidate %+v", c)
	}
}

func TestPruneClockBackwardsAborts(t *testing.T) {
	t.Parallel()
	h := scanGapHarness(t)
	h.seedStamped(t, -time.Hour)
	if err := h.m.storage.SetLastScan(h.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	before := h.ledger(t)

	r, err := h.m.RunPrune(context.Background())

	if err == nil || r.Aborted != "clock went backwards" || len(h.handed) != 0 {
		t.Fatalf("err=%v report=%+v", err, r)
	}
	if got := h.ledger(t); !got["bbbb"].Equal(before["bbbb"]) || len(got) != 1 {
		t.Fatalf("ledger changed: %v", got)
	}
	if ls, _, _ := h.m.storage.LastScan(); !ls.Equal(h.now.Add(time.Hour)) {
		t.Fatalf("last scan changed: %v", ls)
	}
	if !strings.Contains(h.logs.String(), `"message":"prune aborted"`) {
		t.Fatalf("no abort log: %s", h.logs.String())
	}
}

func TestPruneCancelledRunLeavesNoTrace(t *testing.T) {
	t.Parallel()
	h := scanGapHarness(t)
	h.run(t)
	good := h.m.LastPruneReport()
	scan0, _, _ := h.m.storage.LastScan()
	h.now = h.now.Add(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := h.m.RunPrune(ctx)

	if err == nil {
		t.Fatal("cancelled run returned no error")
	}
	if strings.Contains(h.logs.String(), "prune aborted") {
		t.Fatalf("cancel logged as abort: %s", h.logs.String())
	}
	if scan1, _, _ := h.m.storage.LastScan(); !scan1.Equal(scan0) {
		t.Fatalf("last scan moved %v -> %v", scan0, scan1)
	}
	if got := h.m.LastPruneReport(); !got.StartedAt.Equal(good.StartedAt) {
		t.Fatalf("last report replaced: %+v", got)
	}
}

func TestPruneIgnoresMigrationStatusRecord(t *testing.T) {
	t.Parallel()
	h := scanGapHarness(t)
	if err := h.m.storage.SaveMigrationStatus(&storage.SystemMigrationStatus{}); err != nil {
		t.Fatal(err)
	}

	r := h.run(t)

	if r.Aborted != "" || len(r.Candidates) != 1 || r.Candidates[0].InfoHash != "bbbb" {
		t.Fatalf("report %+v", r)
	}
	if _, ok := h.ledger(t)[""]; ok || len(h.ledger(t)) != 1 {
		t.Fatalf("ledger %v", h.ledger(t))
	}
}

func TestPruneExcludedEntriesAreNotStamped(t *testing.T) {
	t.Parallel()
	h := scanGapHarness(t)
	inc := h.addEntry(t, "inc1", func(e *storage.Entry) { e.IsComplete = false })

	h.run(t)
	h.age(t, 8*24*time.Hour)
	r := h.run(t)

	c := candidate(t, r, "inc1")
	if c.Excluded != pruneExcludedIncomplete || !c.UnreferencedSince.IsZero() || !c.EligibleAt.IsZero() {
		t.Fatalf("candidate %+v", c)
	}
	if _, ok := h.ledger(t)["inc1"]; ok {
		t.Fatal("excluded entry stamped")
	}

	inc.IsComplete = true
	if err := h.m.storage.AddOrUpdate(inc); err != nil {
		t.Fatal(err)
	}
	r = h.run(t)
	c = candidate(t, r, "inc1")
	if !c.UnreferencedSince.Equal(h.now) || c.eligible(h.now) {
		t.Fatalf("after completing: %+v", c)
	}
	h.age(t, pruneGrace)
	if r = h.run(t); !candidate(t, r, "inc1").eligible(h.now) {
		t.Fatalf("not eligible a grace later: %+v", r.Candidates)
	}
}

func TestPruneArrRecoveryRequiredWhenArrsConfigured(t *testing.T) {
	t.Parallel()
	t.Run("arrs configured", func(t *testing.T) {
		t.Parallel()
		h := scanGapHarness(t)
		h.edit(t, func(c *config.Config) { c.Arrs = []config.Arr{{Name: "sonarr"}} })
		h.m.SetArrRecovery(nil)

		r, err := h.m.RunPrune(context.Background())

		if err == nil || r.Aborted != "arr recovery unavailable" || len(h.ledger(t)) != 0 {
			t.Fatalf("err=%v report=%+v", err, r)
		}
	})
	t.Run("no arrs", func(t *testing.T) {
		t.Parallel()
		h := scanGapHarness(t)
		h.m.SetArrRecovery(nil)
		if r := h.run(t); r.Aborted != "" {
			t.Fatalf("report %+v", r)
		}
	})
}

func TestPruneMissingReferencedEntries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		missing int
		abort   bool
	}{{1, false}, {2, false}, {3, true}} {
		t.Run(fmt.Sprintf("%d of 200", tc.missing), func(t *testing.T) {
			t.Parallel()
			h := newPruneHarness(t, config.PruneModeDelete)
			for i := range 200 {
				hash := fmt.Sprintf("h%03d", i)
				ghost := i < tc.missing
				e := h.addEntry(t, hash, func(e *storage.Entry) {
					if ghost {
						e.Files["a.mkv"].InfoHash = "ghost" + hash
					}
				})
				h.reference(t, e)
			}
			h.addEntry(t, "orph", nil)

			r, _ := h.m.RunPrune(context.Background())

			if tc.abort {
				assertAborted(t, h, r)
				return
			}
			if r.Aborted != "" || r.MissingReferenced != tc.missing {
				t.Fatalf("report %+v", r)
			}
		})
	}
}

func TestPruneStoredReportDoesNotShareReturnedSlice(t *testing.T) {
	t.Parallel()
	h := scanGapHarness(t)

	r := h.run(t)
	r.Candidates[0].InfoHash = "mutated"

	if h.m.LastPruneReport().Candidates[0].InfoHash != "bbbb" {
		t.Fatal("stored report shares the returned backing array")
	}
}
