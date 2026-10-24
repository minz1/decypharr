package manager

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// A provider at its active-download cap was retried every 30s forever; back off up to 10 minutes.
func TestThrottledRetryBacksOff(t *testing.T) {
	t.Parallel()
	for attempts, want := range map[int]time.Duration{
		0: 30 * time.Second, 1: time.Minute, 2: 2 * time.Minute, 4: 8 * time.Minute, 5: 10 * time.Minute, 60: 10 * time.Minute,
	} {
		if got := throttledRetryDelay(attempts); got != want {
			t.Errorf("throttledRetryDelay(%d) = %s, want %s", attempts, got, want)
		}
	}
}

func TestThrottledJobCountsItsRetries(t *testing.T) {
	t.Parallel()
	m := withTestConfig(t, &Manager{logger: zerolog.Nop()})
	m.jobQueue = NewJobQueue(t.Context(), 1, func(context.Context, *Job) {}, zerolog.Nop())
	t.Cleanup(m.jobQueue.Close)
	job := &Job{ID: "job", Type: JobTypeTorrent}
	for range 3 {
		m.handleJobError(job, fmt.Errorf("submit: %w", customerror.ErrTooManyActiveDownloads))
	}
	if job.ThrottledRetries != 3 {
		t.Fatalf("ThrottledRetries = %d, want 3", job.ThrottledRetries)
	}
}

// RD is tried first on media-0: its "blocked" must not hide TorBox's full slots behind it.
func TestThrottleSeenBehindAnotherProviderError(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("failed to process torrent: %w", errors.Join(
		fmt.Errorf("realdebrid: %w", customerror.ErrTorrentBlocked),
		fmt.Errorf("torbox queued it: %w", customerror.ErrTooManyActiveDownloads),
	))
	if !isTooManyActiveDownloads(err) {
		t.Fatalf("isTooManyActiveDownloads(%v) = false, want true", err)
	}
}

func startRecordingQueue(t *testing.T) (*JobQueue, chan string) {
	t.Helper()
	ran := make(chan string, 8)
	q := NewJobQueue(t.Context(), 1, func(_ context.Context, job *Job) { ran <- job.ID }, zerolog.Nop())
	t.Cleanup(q.Close)
	return q, ran
}

func drained(ran chan string) []string {
	var ids []string
	for {
		select {
		case id := <-ran:
			ids = append(ids, id)
		default:
			return ids
		}
	}
}

func TestFreedSlotWakesTheOldestJobWaitingOnThatProvider(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q, ran := startRecordingQueue(t)
		q.WaitForSlot(&Job{ID: "torbox-only"}, []string{"torbox"}, time.Hour)
		q.WaitForSlot(&Job{ID: "first"}, []string{"realdebrid"}, time.Hour)
		q.WaitForSlot(&Job{ID: "second"}, []string{"torbox", "realdebrid"}, time.Hour)
		q.SlotFreed("realdebrid")
		synctest.Wait()
		if got := drained(ran); !slices.Equal(got, []string{"first"}) {
			t.Fatalf("after one freed realdebrid slot ran %v, want [first]", got)
		}
		q.SlotFreed("realdebrid")
		synctest.Wait()
		if got := drained(ran); !slices.Equal(got, []string{"second"}) {
			t.Fatalf("after a second freed realdebrid slot ran %v, want [second]", got)
		}
	})
}

func TestSlotFreedOnAnotherProviderWakesNoJob(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q, ran := startRecordingQueue(t)
		q.WaitForSlot(&Job{ID: "job"}, []string{"realdebrid"}, time.Hour)
		q.SlotFreed("torbox")
		synctest.Wait()
		if got := drained(ran); len(got) != 0 {
			t.Fatalf("a torbox slot woke %v, want none", got)
		}
	})
}

// Slots used outside decypharr free with no wake; the backoff still retries.
func TestWaitingJobRetriesAfterItsBackoffWithoutAWake(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q, ran := startRecordingQueue(t)
		q.WaitForSlot(&Job{ID: "job"}, []string{"realdebrid"}, time.Minute)
		time.Sleep(time.Minute - time.Second)
		synctest.Wait()
		if got := drained(ran); len(got) != 0 {
			t.Fatalf("ran %v before the backoff, want none", got)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if got := drained(ran); !slices.Equal(got, []string{"job"}) {
			t.Fatalf("after the backoff ran %v, want [job]", got)
		}
		q.WaitForSlot(&Job{ID: "later"}, []string{"realdebrid"}, time.Hour)
		q.SlotFreed("realdebrid")
		synctest.Wait()
		if got := drained(ran); !slices.Equal(got, []string{"later"}) {
			t.Fatalf("a freed slot after the retry ran %v, want [later]", got)
		}
	})
}

// A backoff timer firing as a slot frees must not swallow the slot: the timed-out job carries it, or the next waiter gets it.
func TestTimerWakeRacingSlotFreedLosesNoSlot(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		type run struct{ id, wokenBy string }
		ran := make(chan run, 4)
		q := NewJobQueue(
			t.Context(),
			1,
			func(_ context.Context, job *Job) { ran <- run{job.ID, job.wokenBy} },
			zerolog.Nop(),
		)
		defer q.Close()
		for range 200 {
			q.WaitForSlot(&Job{ID: "timed"}, []string{"realdebrid"}, time.Minute)
			q.WaitForSlot(&Job{ID: "next"}, []string{"realdebrid"}, time.Hour)
			time.Sleep(time.Minute)
			q.SlotFreed("realdebrid")
			synctest.Wait()
			var got []run
			for len(ran) > 0 {
				got = append(got, <-ran)
			}
			slices.SortFunc(got, func(a, b run) int { return strings.Compare(a.id, b.id) })
			carried := slices.Equal(got, []run{{"timed", "realdebrid"}})
			passed := slices.Equal(got, []run{{"next", "realdebrid"}, {"timed", ""}})
			if !carried && !passed {
				t.Fatalf("ran %v: the freed slot was lost", got)
			}
			if carried {
				q.SlotFreed("realdebrid")
				synctest.Wait()
				<-ran
			}
		}
	})
}

type throttlingProvider struct {
	debrid.Client

	name    string
	err     error
	submits atomic.Int64
}

func (p *throttlingProvider) Config() config.Debrid {
	return config.Debrid{Name: p.name, Provider: p.name}
}
func (p *throttlingProvider) Logger() zerolog.Logger { return zerolog.Nop() }
func (p *throttlingProvider) SubmitMagnet(*types.Torrent) (*types.Torrent, error) {
	p.submits.Add(1)
	return nil, p.err
}

// The job waits on the provider that throttled it, not on one that refused it for another reason.
func TestThrottledSubmitWaitsOnTheProviderThatThrottledIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := withTestConfig(t, &Manager{logger: zerolog.Nop(), clients: xsync.NewMap[string, debrid.Client]()})
		m.clients.Store("realdebrid", &throttlingProvider{name: "realdebrid", err: customerror.ErrTorrentBlocked})
		m.clients.Store("torbox", &throttlingProvider{
			name: "torbox", err: fmt.Errorf("torbox queued it: %w", customerror.ErrTooManyActiveDownloads),
		})
		var ran chan string
		m.jobQueue, ran = startRecordingQueue(t)
		req := &ImportRequest{Magnet: &utils.Magnet{InfoHash: "hash", Name: "name"}}
		_, err := m.SendToDebrid(t.Context(), req)
		m.handleJobError(&Job{ID: "job", Type: JobTypeTorrent, Request: req}, err)
		m.slotFreed("realdebrid")
		synctest.Wait()
		if got := drained(ran); len(got) != 0 {
			t.Fatalf("a realdebrid slot woke %v, want none", got)
		}
		m.slotFreed("torbox")
		synctest.Wait()
		if got := drained(ran); !slices.Equal(got, []string{"job"}) {
			t.Fatalf("a torbox slot ran %v, want [job]", got)
		}
	})
}

// A torrent that finished or failed on its provider freed an active-download slot there.
func TestQueuedTorrentLeavingItsProviderSlotWakesAWaitingJob(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		provider *failingStatusProvider
		wantWake bool
	}{
		"provider no longer has it": {&failingStatusProvider{err: customerror.ErrTorrentNotFound}, true},
		"provider status is error":  {&failingStatusProvider{status: types.TorrentStatusError}, true},
		"still downloading":         {&failingStatusProvider{status: types.TorrentStatusDownloading}, false},
		"status check timed out":    {&failingStatusProvider{err: context.DeadlineExceeded}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, entry := newDroppedTestManager(t, &fakeArrRecovery{})
			m.processingEntries = xsync.NewMap[string, struct{}]()
			m.store = testConfigStore(t, nil)
			m.clients.Store("provider", tc.provider)
			entry.Protocol, entry.ActiveProvider = config.ProtocolTorrent, "provider"
			entry.State = storage.EntryStateDownloading
			entry.Providers = map[string]*storage.ProviderEntry{"provider": {Provider: "provider", ID: "id"}}
			m.jobQueue, _ = startRecordingQueue(t)
			m.jobQueue.WaitForSlot(&Job{ID: "waiting"}, []string{"provider"}, time.Hour)
			m.processQueuedTorrent(entry)
			if woke := waitingJobs(m.jobQueue) == 0; woke != tc.wantWake {
				t.Fatalf("woke a waiting job = %v, want %v", woke, tc.wantWake)
			}
		})
	}
}

// Removing an unfinished placement from its provider frees the slot it held there; a finished one held none.
func TestRemovingAPlacementWakesAJobWaitingOnItsProvider(t *testing.T) {
	t.Parallel()
	for progress, want := range map[float64][]string{0.4: {"waiting"}, 1: nil} {
		synctest.Test(t, func(t *testing.T) {
			m := withTestConfig(t, &Manager{logger: zerolog.Nop(), clients: xsync.NewMap[string, debrid.Client]()})
			m.clients.Store("provider", &failingStatusProvider{})
			var ran chan string
			m.jobQueue, ran = startRecordingQueue(t)
			m.jobQueue.WaitForSlot(&Job{ID: "waiting"}, []string{"provider"}, time.Hour)
			placement := &storage.ProviderEntry{Provider: "provider", ID: "id", Progress: progress}
			if err := m.RemoveFromProvider(placement); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			if got := drained(ran); !slices.Equal(got, want) {
				t.Fatalf("progress %v: ran %v, want %v", progress, got, want)
			}
		})
	}
}

// A woken job the provider throttles again keeps its place ahead of jobs throttled after it.
func TestRethrottledJobKeepsItsPlaceInLine(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q, ran := startRecordingQueue(t)
		older := NewJob(JobTypeTorrent, nil)
		older.ID = "older"
		time.Sleep(time.Second)
		newer := NewJob(JobTypeTorrent, nil)
		newer.ID = "newer"
		q.WaitForSlot(newer, []string{"realdebrid"}, time.Hour)
		q.WaitForSlot(older, []string{"realdebrid"}, time.Hour)
		q.SlotFreed("realdebrid")
		synctest.Wait()
		if got := drained(ran); !slices.Equal(got, []string{"older"}) {
			t.Fatalf("ran %v, want [older]", got)
		}
	})
}

// A torrent throttled at add waits for a slot too, instead of a worker resubmitting it at once.
func TestTorrentThrottledAtAddWaitsForASlot(t *testing.T) {
	t.Parallel()
	m, _ := newDroppedTestManager(t, nil)
	m = withTestConfig(t, m)
	m.processingEntries = xsync.NewMap[string, struct{}]()
	provider := &throttlingProvider{name: "torbox", err: customerror.ErrTooManyActiveDownloads}
	m.clients.Store("torbox", provider)
	m.jobQueue = NewJobQueue(t.Context(), 1, m.processJob, zerolog.Nop())
	t.Cleanup(m.jobQueue.Close)
	req := &ImportRequest{
		Magnet: &utils.Magnet{
			InfoHash: "hash",
			Name:     "name",
		},
		Arr:            arr.Arr{Name: "sonarr"},
		DownloadFolder: t.TempDir(),
	}
	if err := m.addNewTorrent(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	m.jobQueue.mu.Lock()
	queued, waiting := len(m.jobQueue.jobs), len(m.jobQueue.waiters)
	m.jobQueue.mu.Unlock()
	if got := provider.submits.Load(); got != 1 || queued != 0 || waiting != 1 {
		t.Fatalf(
			"before a slot freed: submits = %d, queued jobs = %d, waiting = %d; want 1, 0, 1",
			got,
			queued,
			waiting,
		)
	}
	m.slotFreed("torbox")
	deadline := time.Now().Add(2 * time.Second)
	for provider.submits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := provider.submits.Load(); got != 2 {
		t.Fatalf("submits after a slot freed = %d, want 2", got)
	}
}

// A woken job the same provider throttles again waits once more and leaves the next waiter alone.
func TestWokenJobThrottledAgainWaitsAndPassesNothingOn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, _ := newDroppedTestManager(t, nil)
		m = withTestConfig(t, m)
		m.processingEntries = xsync.NewMap[string, struct{}]()
		provider := &throttlingProvider{name: "torbox", err: customerror.ErrTooManyActiveDownloads}
		m.clients.Store("torbox", provider)
		m.jobQueue = NewJobQueue(t.Context(), 1, m.processJob, zerolog.Nop())
		t.Cleanup(m.jobQueue.Close)
		for _, hash := range []string{"first", "second"} {
			req := &ImportRequest{
				Magnet:         &utils.Magnet{InfoHash: hash, Name: hash},
				Arr:            arr.Arr{Name: "sonarr"},
				DownloadFolder: t.TempDir(),
			}
			if err := m.addNewTorrent(t.Context(), req); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		m.slotFreed("torbox")
		synctest.Wait()
		if got, waiting := provider.submits.Load(), waitingJobs(m.jobQueue); got != 3 || waiting != 2 {
			t.Fatalf("after one freed slot: submits = %d, waiting = %d; want 3, 2", got, waiting)
		}
	})
}

func TestWokenJobKnowsWhichProvidersSlotWokeIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ran := make(chan string, 2)
		record := func(_ context.Context, job *Job) { ran <- job.ID + "=" + job.wokenBy }
		q := NewJobQueue(t.Context(), 1, record, zerolog.Nop())
		t.Cleanup(q.Close)
		q.WaitForSlot(&Job{ID: "woken"}, []string{"torbox", "realdebrid"}, time.Hour)
		q.WaitForSlot(&Job{ID: "timed"}, []string{"torbox"}, time.Minute)
		q.SlotFreed("realdebrid")
		time.Sleep(time.Minute)
		synctest.Wait()
		got := drained(ran)
		slices.Sort(got)
		if want := []string{"timed=", "woken=realdebrid"}; !slices.Equal(got, want) {
			t.Fatalf("ran %v, want %v", got, want)
		}
	})
}

func waitingJobs(q *JobQueue) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiters)
}

// A woken job that doesn't take the freed slot hands it to the next job waiting on that provider.
func TestWokenJobThatDoesNotUseTheSlotPassesItOn(t *testing.T) {
	t.Parallel()
	throttledOn := func(providers ...string) error {
		return &throttledError{providers: providers, err: customerror.ErrTooManyActiveDownloads}
	}
	for name, tc := range map[string]struct {
		job      *Job
		err      error
		wantWake bool
	}{
		"placed on that provider": {&Job{wokenBy: "torbox", DebridTorrent: &types.Torrent{Debrid: "torbox"}}, nil, false},
		"placed there already cached": {
			&Job{wokenBy: "torbox", DebridTorrent: &types.Torrent{Debrid: "torbox", Status: types.TorrentStatusDownloaded}},
			nil,
			true,
		},
		"placed on another provider": {&Job{wokenBy: "torbox", DebridTorrent: &types.Torrent{Debrid: "realdebrid"}}, nil, true},
		"removed from the queue":     {&Job{wokenBy: "torbox"}, nil, true},
		"throttled there again":      {&Job{wokenBy: "torbox"}, throttledOn("realdebrid", "torbox"), false},
		"throttled elsewhere only":   {&Job{wokenBy: "torbox"}, throttledOn("realdebrid"), true},
		"failed otherwise":           {&Job{wokenBy: "torbox"}, errors.New("boom"), true},
		"not woken by a slot":        {&Job{}, errors.New("boom"), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := &Manager{logger: zerolog.Nop()}
			m.jobQueue, _ = startRecordingQueue(t)
			m.jobQueue.WaitForSlot(&Job{ID: "next"}, []string{"torbox"}, time.Hour)
			m.passOnUnusedSlot(tc.job, tc.job.wokenBy, tc.err)
			if woke := waitingJobs(m.jobQueue) == 0; woke != tc.wantWake {
				t.Fatalf("woke the next job = %v, want %v", woke, tc.wantWake)
			}
		})
	}
}

func TestWokenJobRemovedFromTheQueuePassesItsSlotOn(t *testing.T) {
	t.Parallel()
	m, _ := newDroppedTestManager(t, nil)
	m.jobQueue, _ = startRecordingQueue(t)
	m.jobQueue.WaitForSlot(&Job{ID: "next"}, []string{"torbox"}, time.Hour)
	job := &Job{ID: "gone", Type: JobTypeTorrent, Entry: &storage.Entry{InfoHash: "gone"}, wokenBy: "torbox"}
	m.processJob(t.Context(), job)
	if waitingJobs(m.jobQueue) != 0 {
		t.Fatal("the next job waiting on torbox is still waiting")
	}
}

// An errored torrent freed its slot when it failed; deleting it later frees nothing more.
func TestErroredTorrentWakesOneJobNotTwo(t *testing.T) {
	t.Parallel()
	m, entry := newDroppedTestManager(t, &fakeArrRecovery{})
	m.processingEntries = xsync.NewMap[string, struct{}]()
	m.store = testConfigStore(t, nil)
	m.clients.Store("provider", &failingStatusProvider{status: types.TorrentStatusError})
	entry.ActiveProvider = "provider"
	entry.State = storage.EntryStateDownloading
	entry.Providers = map[string]*storage.ProviderEntry{"provider": {Provider: "provider", ID: "id", Progress: 0.4}}
	m.jobQueue, _ = startRecordingQueue(t)
	m.jobQueue.WaitForSlot(&Job{ID: "first"}, []string{"provider"}, time.Hour)
	m.jobQueue.WaitForSlot(&Job{ID: "second"}, []string{"provider"}, time.Hour)
	m.processQueuedTorrent(entry)
	saved, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.RemoveFromProvider(saved.Providers["provider"]); err != nil {
		t.Fatal(err)
	}
	if got := waitingJobs(m.jobQueue); got != 1 {
		t.Fatalf("waiting jobs = %d, want 1", got)
	}
}

func TestPruningAPlacementWakesAJobOnlyWhenItFreedASlot(t *testing.T) {
	t.Parallel()
	alreadyGone := func(string) error { return errors.New("delete failed") }
	for _, tc := range []struct {
		name      string
		placement storage.ProviderEntry
		deleteErr func(string) error
		wantWoken int
	}{
		{"mid download", storage.ProviderEntry{Progress: 0.4, Status: types.TorrentStatusDownloading}, nil, 1},
		{"downloaded", storage.ProviderEntry{Progress: 1, Status: types.TorrentStatusDownloaded}, nil, 0},
		{"errored", storage.ProviderEntry{Progress: 0.4, Status: types.TorrentStatusError}, nil, 0},
		{"already gone from the provider", storage.ProviderEntry{Progress: 0.4}, alreadyGone, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, _ := newDroppedTestManager(t, nil)
			notFound := func(string) (*types.Torrent, error) { return nil, customerror.ErrTorrentNotFound }
			m.clients.Store("provider", &fakePruneProvider{deleteErr: tc.deleteErr, lookup: notFound})
			m.jobQueue, _ = startRecordingQueue(t)
			m.jobQueue.WaitForSlot(&Job{ID: "next"}, []string{"provider"}, time.Hour)
			placement := tc.placement
			placement.Provider, placement.ID = "provider", "id"
			e := &storage.Entry{
				InfoHash:  "pruned",
				Providers: map[string]*storage.ProviderEntry{"provider": &placement},
			}
			if _, err := m.pruneRemovePlacements(e); err != nil {
				t.Fatal(err)
			}
			if got := 1 - waitingJobs(m.jobQueue); got != tc.wantWoken {
				t.Fatalf("woken jobs = %d, want %d", got, tc.wantWoken)
			}
		})
	}
}

func TestCompletedQueuedTorrentWakesOneJobAndStaysDueForSync(t *testing.T) {
	t.Parallel()
	m := newShutdownTestManager(t, filepath.Join(t.TempDir(), "db"))
	m.clients.Store("provider", &failingStatusProvider{status: types.TorrentStatusDownloaded})
	entry := &storage.Entry{
		InfoHash:       "done",
		Name:           "done",
		Protocol:       config.ProtocolTorrent,
		ActiveProvider: "provider",
		State:          storage.EntryStateDownloading,
		Action:         config.DownloadActionNone,
		Providers: map[string]*storage.ProviderEntry{"provider": {
			Provider: "provider",
			ID:       "id",
			Status:   types.TorrentStatusDownloading,
			Files:    map[string]*storage.ProviderFile{"a.mkv": {ID: "1"}},
		}},
		Files: map[string]*storage.File{},
	}
	if err := m.queue.Add(entry); err != nil {
		t.Fatal(err)
	}
	m.jobQueue, _ = startRecordingQueue(t)
	m.jobQueue.WaitForSlot(&Job{ID: "first"}, []string{"provider"}, time.Hour)
	m.jobQueue.WaitForSlot(&Job{ID: "second"}, []string{"provider"}, time.Hour)
	m.processQueuedTorrent(entry)
	if waitingJobs(m.jobQueue) != 1 {
		t.Fatal("a completed download did not wake exactly one job waiting on its provider")
	}
	stored, err := m.storage.Get(entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	// The add-time files carry no links; only a sync re-fetch fills them in.
	if !stored.Providers["provider"].NeedsUpdate(&types.Torrent{ID: "id", Status: types.TorrentStatusDownloaded}) {
		t.Fatal("the stored placement looks current, so sync never re-fetches its links")
	}
}

type finishingProvider struct {
	failingStatusProvider

	debrid string
	files  map[string]types.File
}

func (p *finishingProvider) CheckStatus(torrent *types.Torrent) (*types.Torrent, error) {
	torrent.Status = types.TorrentStatusDownloaded
	if p.debrid != "" {
		torrent.Debrid = p.debrid
	}
	torrent.Files = p.files
	return torrent, nil
}

func linkedFiles() map[string]types.File {
	return map[string]types.File{
		"a.mkv": {Name: "a.mkv", ID: "1", Link: "https://rd/a"},
		"b.mkv": {Name: "b.mkv", ID: "2", Link: "https://rd/b"},
	}
}

func finishQueuedTorrent(
	t *testing.T,
	provider *finishingProvider,
	entryFiles map[string]*storage.File,
	added time.Time,
) (*Manager, *storage.Entry) {
	t.Helper()
	m := newShutdownTestManager(t, filepath.Join(t.TempDir(), "db"))
	m.clients.Store("provider", provider)
	entry := &storage.Entry{
		InfoHash:       "done",
		Name:           "done",
		Protocol:       config.ProtocolTorrent,
		ActiveProvider: "provider",
		State:          storage.EntryStateDownloading,
		Action:         config.DownloadActionNone,
		Providers: map[string]*storage.ProviderEntry{"provider": {
			Provider: "provider",
			ID:       "id",
			AddedAt:  added,
			Status:   types.TorrentStatusDownloading,
			Files:    map[string]*storage.ProviderFile{"a.mkv": {ID: "1"}, "b.mkv": {ID: "2"}},
		}},
		Files: entryFiles,
	}
	if err := m.queue.Add(entry); err != nil {
		t.Fatal(err)
	}
	m.jobQueue, _ = startRecordingQueue(t)
	m.processQueuedTorrent(entry)
	stored, err := m.storage.Get(entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	return m, stored
}

func addTimeFiles() map[string]*storage.File {
	return map[string]*storage.File{"a.mkv": {Name: "a.mkv"}, "b.mkv": {Name: "b.mkv"}}
}

// The provider leaves Debrid unset (DebridLink), so the queued entry's provider names the placement.
func TestFinishedQueuedTorrentStoresItsLinksBeforeTheArrImports(t *testing.T) {
	t.Parallel()
	added := time.Now().Add(-time.Hour).Truncate(time.Second)
	entryFiles := addTimeFiles()
	entryFiles["b.mkv"].Deleted = true
	_, stored := finishQueuedTorrent(t, &finishingProvider{files: linkedFiles()}, entryFiles, added)
	if !stored.Files["b.mkv"].Deleted {
		t.Fatal("the finished torrent overwrote files the entry already knew")
	}
	placement := stored.Providers["provider"]
	if placement.Files["a.mkv"].Link != "https://rd/a" || placement.Files["b.mkv"].Link != "https://rd/b" {
		t.Fatalf("stored placement files = %+v, want the finished torrent's links", placement.Files)
	}
	if placement.NeedsUpdate(&types.Torrent{ID: "id", Status: types.TorrentStatusDownloaded}) {
		t.Fatal("a placement holding every link still asks sync for a re-fetch")
	}
	if placement.HoldsSlot() || !placement.AddedAt.Equal(added) || placement.DownloadedAt == nil {
		t.Fatalf("placement = %+v, want finished, downloaded and added at %v", placement, added)
	}
}

func TestFinishedQueuedTorrentLeftForSync(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		provider   *finishingProvider
		entryFiles map[string]*storage.File
	}{
		// Real-Debrid can report a torrent downloaded before every file has a link.
		"a file without a link": {
			provider: &finishingProvider{files: map[string]types.File{
				"a.mkv": {Name: "a.mkv", ID: "1", Link: "https://rd/a"},
				"b.mkv": {Name: "b.mkv", ID: "2"},
			}},
			entryFiles: addTimeFiles(),
		},
		// TorBox reports "completed" before DownloadFinished, when its files carry no torbox:// links yet.
		"an entry without files whose torrent has no links": {
			provider: &finishingProvider{files: map[string]types.File{
				"a.mkv": {TorrentID: "id", ID: "1", Name: "a.mkv", Path: "a.mkv", Size: 1},
			}},
			entryFiles: map[string]*storage.File{},
		},
		"another provider's torrent": {
			provider:   &finishingProvider{debrid: "other", files: linkedFiles()},
			entryFiles: addTimeFiles(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			knownFiles := len(tc.entryFiles)
			_, stored := finishQueuedTorrent(t, tc.provider, tc.entryFiles, time.Now())
			if len(stored.Providers) != 1 {
				got := slices.Collect(maps.Keys(stored.Providers))
				t.Fatalf("placements = %v, want only the queued provider's", got)
			}
			if len(stored.Files) != knownFiles {
				t.Fatalf(
					"entry files = %v, want the entry's own %d",
					slices.Collect(maps.Keys(stored.Files)),
					knownFiles,
				)
			}
			placement := stored.Providers["provider"]
			if !placement.NeedsUpdate(&types.Torrent{ID: "id", Status: types.TorrentStatusDownloaded}) {
				t.Fatal("the stored placement looks current, so sync never re-fetches it")
			}
			if placement.Files["a.mkv"].Link != "" {
				t.Fatalf("stored placement files = %+v, want the add-time files kept", placement.Files)
			}
		})
	}
}

// Uncached adds (AllDebrid, Premiumize, RD magnet_conversion, TorBox metaDL) know no files until they finish.
func TestFinishedQueuedTorrentFillsAnEntryWithoutFiles(t *testing.T) {
	t.Parallel()
	for name, files := range map[string]map[string]types.File{
		"realdebrid": {
			"a.mkv": {TorrentID: "id", ID: "1", Name: "a.mkv", Path: "a.mkv", Size: 1, Link: "https://rd/a"},
			"b.mkv": {TorrentID: "id", ID: "2", Name: "b.mkv", Path: "b.mkv", Size: 2, Link: "https://rd/b"},
		},
		"alldebrid": {
			"a.mkv": {TorrentID: "id", ID: "1", Name: "a.mkv", Path: "Pack/a.mkv", Size: 1, Link: "https://ad/a"},
		},
		"premiumize": {
			"a.mkv":     {TorrentID: "id", ID: "1", Name: "a.mkv", Path: "a.mkv", Size: 1, Link: "https://pm/a"},
			"Sub/a.mkv": {TorrentID: "id", ID: "2", Name: "Sub/a.mkv", Path: "Sub/a.mkv", Size: 2, Link: "https://pm/b"},
		},
		"debridlink": {
			"a.mkv": {TorrentID: "id", ID: "1", Name: "a.mkv", Path: "a.mkv", Size: 1, Link: "https://dl/a",
				Generated: time.Now()},
		},
		"torbox download finished": {
			"a.mkv": {TorrentID: "id", ID: "1", Name: "a.mkv", Path: "a.mkv", Size: 1, Link: "torbox://id/1"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, stored := finishQueuedTorrent(
				t,
				&finishingProvider{files: files},
				map[string]*storage.File{},
				time.Now(),
			)
			if len(stored.Files) != len(files) {
				t.Fatalf(
					"entry files = %v, want the finished torrent's %d",
					slices.Collect(maps.Keys(stored.Files)),
					len(files),
				)
			}
			placement := stored.Providers["provider"]
			for fileName, f := range files {
				got := stored.Files[fileName]
				if got == nil || got.Name != f.Name || got.Size != f.Size || got.InfoHash != stored.InfoHash {
					t.Fatalf("entry file %q = %+v, want name %q size %d", fileName, got, f.Name, f.Size)
				}
				if placement.Files[fileName] == nil || placement.Files[fileName].Link != f.Link {
					t.Fatalf("placement file %q = %+v, want link %q", fileName, placement.Files[fileName], f.Link)
				}
			}
		})
	}
}

func TestFinishedSeasonPackWithoutEntryFilesIsDetected(t *testing.T) {
	t.Parallel()
	m, stored := finishQueuedTorrent(t, &finishingProvider{files: map[string]types.File{
		"Show.S01E01.mkv": {ID: "1", Name: "Show.S01E01.mkv", Size: 1, Link: "https://rd/1"},
		"Show.S02E01.mkv": {ID: "2", Name: "Show.S02E01.mkv", Size: 1, Link: "https://rd/2"},
	}}, map[string]*storage.File{}, time.Now())
	if found, seasons := m.downloader.detectMultiSeason(stored); !found || len(seasons) != 2 {
		t.Fatalf("detected %t with %d seasons, want the finished pack's 2", found, len(seasons))
	}
}

// Real-Debrid's link sync re-fetches a placement only while it lacks links; one stored at finish keeps them.
func TestSyncKeepsTheLinksStoredAtFinish(t *testing.T) {
	t.Parallel()
	m, stored := finishQueuedTorrent(
		t,
		&finishingProvider{files: linkedFiles()},
		map[string]*storage.File{},
		time.Now(),
	)
	listed := &types.Torrent{
		InfoHash: stored.InfoHash, Name: stored.Name, Debrid: "provider", ID: "id",
		Status: types.TorrentStatusDownloaded, Progress: 100, Files: linkedFiles(),
	}
	if stored.Providers["provider"].NeedsUpdate(listed) {
		t.Fatal("sync would re-fetch a placement that already holds every link")
	}
	synced, err := m.processSyncTorrent(listed)
	if err != nil {
		t.Fatal(err)
	}
	placement := synced.Providers["provider"]
	if len(synced.Files) != 2 || placement.Files["a.mkv"].Link != "https://rd/a" ||
		placement.Files["b.mkv"].Link != "https://rd/b" {
		t.Fatalf(
			"after sync: entry files %v, placement files %+v",
			slices.Collect(maps.Keys(synced.Files)),
			placement.Files,
		)
	}
}

// A grab the arr removes mid download must not keep a provider slot nobody polls or wakes on.
func TestDeletingAQueuedTorrentStopsOnlyItsProviderDownload(t *testing.T) {
	t.Parallel()
	downloading := storage.ProviderEntry{Progress: 0.4, Status: types.TorrentStatusDownloading}
	for _, tc := range []struct {
		name        string
		placement   *storage.ProviderEntry
		protocol    config.Protocol
		libraryID   string
		complete    bool
		wantDeleted []string
	}{
		{"downloading", &downloading, config.ProtocolTorrent, "", false, []string{"id"}},
		{"downloaded", &storage.ProviderEntry{Progress: 1, Status: types.TorrentStatusDownloaded}, config.ProtocolTorrent, "", false, nil},
		{"errored", &storage.ProviderEntry{Progress: 0.4, Status: types.TorrentStatusError}, config.ProtocolTorrent, "", false, nil},
		{"waiting for a slot", nil, config.ProtocolTorrent, "", false, nil},
		{"shared with a library entry", &downloading, config.ProtocolTorrent, "id", false, nil},
		{"library entry on another torrent", &downloading, config.ProtocolTorrent, "other", false, []string{"id"}},
		{"nzb", &downloading, config.ProtocolNZB, "", false, nil},
		{"completed with a stale placement", &downloading, config.ProtocolTorrent, "", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, _ := newDroppedTestManager(t, nil)
			fake := &fakePruneProvider{}
			m.clients.Store("provider", fake)
			m.jobQueue, _ = startRecordingQueue(t)
			m.jobQueue.WaitForSlot(&Job{ID: "next"}, []string{"provider"}, time.Hour)
			addRemovedGrab(t, m, tc.placement, tc.protocol, tc.libraryID, tc.complete)
			if err := m.queue.Delete("grab", true, m.StopProviderDownload); err != nil {
				t.Fatal(err)
			}
			if m.queue.Contains("grab") {
				t.Fatal("queue entry kept")
			}
			if got := fake.deletedIDs(); !slices.Equal(got, tc.wantDeleted) {
				t.Fatalf("deleted provider torrents = %v, want %v", got, tc.wantDeleted)
			}
			if got, want := 1-waitingJobs(m.jobQueue), len(tc.wantDeleted); got != want {
				t.Fatalf("woken jobs = %d, want %d", got, want)
			}
		})
	}
}

func addRemovedGrab(
	t *testing.T,
	m *Manager,
	placement *storage.ProviderEntry,
	protocol config.Protocol,
	libraryID string,
	complete bool,
) {
	t.Helper()
	e := &storage.Entry{
		InfoHash:   "grab",
		Name:       "grab",
		Category:   "sonarr",
		Protocol:   protocol,
		State:      storage.EntryStateDownloading,
		Status:     types.TorrentStatusDownloading,
		Providers:  map[string]*storage.ProviderEntry{},
		IsComplete: complete,
	}
	if placement != nil {
		p := *placement
		p.Provider, p.ID = "provider", "id"
		e.ActiveProvider = "provider"
		e.Providers["provider"] = &p
	}
	if err := m.queue.Add(e); err != nil {
		t.Fatal(err)
	}
	if libraryID == "" {
		return
	}
	lib := &storage.Entry{
		InfoHash:       "grab",
		Name:           "grab",
		Protocol:       config.ProtocolTorrent,
		State:          storage.EntryStatePausedUP,
		ActiveProvider: "provider",
		Providers: map[string]*storage.ProviderEntry{"provider": {
			Provider: "provider", ID: libraryID, Progress: 1, Status: types.TorrentStatusDownloaded,
		}},
	}
	if err := m.storage.AddOrUpdate(lib); err != nil {
		t.Fatal(err)
	}
}
