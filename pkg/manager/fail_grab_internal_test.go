package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// grabManager is a manager whose arr blocklists at most 2 distinct downloads per provider within a short window,
// with a canary that answers verdict.
func grabManager(t *testing.T, recovery ArrRecovery, verdict *atomic.Int64) (*Manager, *atomic.Int64) {
	t.Helper()
	m, _ := newDroppedTestManager(t, recovery)
	m.arr = arr.New(config.NewStore(&config.Config{
		Arrs: []config.Arr{{Name: "sonarr", Host: "http://localhost:8989", Token: "x"}},
		QueueCleanup: config.QueueCleanup{
			BlocklistPerBucket: 2, BlocklistWindow: "1s",
		},
	}), nil, zerolog.Nop())
	var notices atomic.Int64
	m.arr.SetQueueGuardHooks(arr.QueueGuardHooks{
		Canary: func(context.Context, string) arr.CanaryVerdict { return arr.CanaryVerdict(verdict.Load()) },
		Notify: func(string, string) { notices.Add(1) },
	})
	return m, &notices
}

func grabEntry(i int) *storage.Entry {
	return &storage.Entry{
		InfoHash: fmt.Sprintf("hash%03d", i), Name: fmt.Sprintf("Show.S01E%02d", i), Category: "sonarr",
		ActiveProvider: "realdebrid", Protocol: config.ProtocolTorrent,
	}
}

// The grab failure of a never-imported torrent blocklists its release, so it is as limited as queue cleanup's.
func TestFailGrabStormWithAFailingCanaryFailsNothingPastTheFreeLimit(t *testing.T) {
	t.Parallel()
	recovery := &fakeArrRecovery{}
	var verdict atomic.Int64
	verdict.Store(int64(arr.CanaryFail))
	m, notices := grabManager(t, recovery, &verdict)

	held := 0
	for i := range 10 {
		err := m.failGrab(grabEntry(i), recovery, "sonarr", "realdebrid")
		switch {
		case err == nil:
		case errors.Is(err, arr.ErrBlocklistHeld):
			held++
		default:
			t.Fatal(err)
		}
	}
	if len(recovery.fails) != 2 || held != 8 {
		t.Fatalf("grabs failed = %d, held = %d, want the 2 free ones and 8 held", len(recovery.fails), held)
	}
	if notices.Load() != 1 {
		t.Fatalf("notifications = %d, want one for the trip", notices.Load())
	}
}

func TestHeldGrabsAreRetriedLaterAndSentOnce(t *testing.T) {
	t.Parallel()
	recovery := &fakeArrRecovery{}
	var verdict atomic.Int64
	verdict.Store(int64(arr.CanaryFail))
	m, _ := grabManager(t, recovery, &verdict)
	synctest.Test(t, func(t *testing.T) {
		f := runFailer(t, arrFailerDeps{attemptGrab: deliverOnly(m, recovery)})
		start := time.Now()
		for i := range 5 {
			entry := grabEntry(i)
			f.wantGrab(pendingGrab(
				strings.ToLower(entry.InfoHash), "realdebrid", start.Add(time.Duration(i)*time.Millisecond),
			))
		}
		synctest.Wait()
		if got := failsOf(recovery); len(got) != 2 || f.itemCount() != 3 {
			t.Fatalf(
				"grabs failed straight away = %d, kept = %d; want the 2 free ones sent and 3 kept",
				len(got),
				f.itemCount(),
			)
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		if got := failsOf(recovery); len(got) != 2 || f.itemCount() != 3 {
			t.Fatalf("a retry while the canary fails sent %d more", len(got)-2)
		}

		verdict.Store(int64(arr.CanaryPass))
		time.Sleep(10 * time.Second)
		synctest.Wait()
		got := failsOf(recovery)
		if len(got) != 5 || f.itemCount() != 0 {
			t.Fatalf("grabs failed = %d, kept = %d, want all 5 sent and none kept", len(got), f.itemCount())
		}
		for i, fail := range got {
			if want := grabEntry(i).InfoHash; fail[2] != want {
				t.Errorf("grab %d sent for %s, want %s: each exactly once, oldest first", i, fail[2], want)
			}
		}
	})
}

// The dropped path returns the hold as an error, so the entry is kept and retried by the next sync.
func TestDroppedGrabHeldByTheGuardIsAnError(t *testing.T) {
	t.Parallel()
	recovery := &fakeArrRecovery{}
	var verdict atomic.Int64
	verdict.Store(int64(arr.CanaryFail))
	m, _ := grabManager(t, recovery, &verdict)
	for i := range 2 {
		if err := m.failGrab(grabEntry(i), recovery, "sonarr", "realdebrid"); err != nil {
			t.Fatal(err)
		}
	}
	err := m.failGrab(grabEntry(9), recovery, "sonarr", "realdebrid")
	if !errors.Is(err, arr.ErrBlocklistHeld) || len(recovery.fails) != 2 {
		t.Fatalf("held grab = %v with %d jobs, want a held error and no third job", err, len(recovery.fails))
	}
}

func TestAnAttemptHungOnTheCanaryNeverHoldsTheCaller(t *testing.T) {
	t.Parallel()
	recovery := &fakeArrRecovery{}
	var verdict atomic.Int64
	m, _ := grabManager(t, recovery, &verdict)
	for i := range 2 {
		if err := m.failGrab(grabEntry(i), recovery, "sonarr", "realdebrid"); err != nil {
			t.Fatal(err)
		}
	}
	called, release, delivered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	m.arr.SetQueueGuardHooks(arr.QueueGuardHooks{
		Canary: func(context.Context, string) arr.CanaryVerdict {
			close(called)
			<-release
			return arr.CanaryPass
		},
		Notify: func(string, string) {},
	})
	inner := deliverOnly(m, recovery)
	f := runFailer(t, arrFailerDeps{attemptGrab: func(ctx context.Context, spec attemptSpec) attemptResult {
		defer close(delivered)
		return inner(ctx, spec)
	}})
	entry := grabEntry(9)
	f.wantGrab(pendingGrab(strings.ToLower(entry.InfoHash), "realdebrid", time.Now()))
	<-called
	close(release)
	<-delivered
}
