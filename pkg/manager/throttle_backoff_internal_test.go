package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
			m.jobQueue.mu.Lock()
			woke := len(m.jobQueue.waiters) == 0
			m.jobQueue.mu.Unlock()
			if woke != tc.wantWake {
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
