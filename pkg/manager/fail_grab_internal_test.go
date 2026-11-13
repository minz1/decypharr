package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
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
		err := m.failGrab(grabEntry(i), recovery, "sonarr", "Provider rejected the torrent")
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

// failBadRelease and the bad-mark recheck do not retry themselves; a held grab is kept and sent exactly once later.
func TestHeldGrabIsRetriedLaterAndSentOnce(t *testing.T) {
	t.Parallel()
	recovery := &fakeArrRecovery{}
	var verdict atomic.Int64
	verdict.Store(int64(arr.CanaryFail))
	m, _ := grabManager(t, recovery, &verdict)

	entries := make([]*storage.Entry, 5)
	for i := range entries {
		entries[i] = grabEntry(i)
		if err := m.queue.Add(entries[i]); err != nil {
			t.Fatal(err)
		}
		failed := grab.State{Key: strings.ToLower(entries[i].InfoHash), Phase: grab.PhaseFailed}
		m.grabs.publish(GrabView{State: failed, Entry: *entries[i]})
		m.failEffects(grab.Grab{Entry: entries[i], State: failed}, grab.FailBadRelease)
	}
	if len(recovery.fails) != 2 {
		t.Fatalf("grabs failed straight away = %d, want the 2 free ones", len(recovery.fails))
	}
	if m.heldGrabCount() != 3 {
		t.Fatalf("held grabs = %d, want 3 kept for a retry", m.heldGrabCount())
	}

	m.retryHeldGrabs(t.Context())
	if len(recovery.fails) != 2 {
		t.Fatalf("a retry while the canary fails sent %d more", len(recovery.fails)-2)
	}

	verdict.Store(int64(arr.CanaryPass))
	deadline := time.Now().Add(5 * time.Second)
	for m.heldGrabCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(1100 * time.Millisecond) // the window and the cached verdict are a second long here
		m.retryHeldGrabs(t.Context())
	}
	m.retryHeldGrabs(t.Context())
	if len(recovery.fails) != 5 || m.heldGrabCount() != 0 {
		t.Fatalf("grabs failed = %d, held = %d, want all 5 sent and none held", len(recovery.fails), m.heldGrabCount())
	}
	seen := map[string]int{}
	for _, fail := range recovery.fails {
		seen[fail[2]]++
	}
	for hash, n := range seen {
		if n != 1 {
			t.Errorf("grab of %s failed %d times, want exactly once", hash, n)
		}
	}
}

// The dropped path returns the hold as an error, so the entry is kept and retried by the next sync.
func TestDroppedGrabHeldByTheGuardIsAnError(t *testing.T) {
	t.Parallel()
	recovery := &fakeArrRecovery{}
	var verdict atomic.Int64
	verdict.Store(int64(arr.CanaryFail))
	m, _ := grabManager(t, recovery, &verdict)
	for i := range 2 {
		if err := m.failGrab(grabEntry(i), recovery, "sonarr", "dropped"); err != nil {
			t.Fatal(err)
		}
	}
	err := m.failGrab(grabEntry(9), recovery, "sonarr", "dropped")
	if !errors.Is(err, arr.ErrBlocklistHeld) || len(recovery.fails) != 2 {
		t.Fatalf("held grab = %v with %d jobs, want a held error and no third job", err, len(recovery.fails))
	}
}

// A failed grab's Arr effects run off the actor: a canary probe that hangs never holds the caller.
func TestFailLaterDoesNotWaitForTheCanary(t *testing.T) {
	t.Parallel()
	recovery := &fakeArrRecovery{}
	var verdict atomic.Int64
	m, _ := grabManager(t, recovery, &verdict)
	for i := range 2 {
		if err := m.failGrab(grabEntry(i), recovery, "sonarr", "free"); err != nil {
			t.Fatal(err)
		}
	}
	called, release := make(chan struct{}), make(chan struct{})
	m.arr.SetQueueGuardHooks(arr.QueueGuardHooks{
		Canary: func(context.Context, string) arr.CanaryVerdict {
			close(called)
			<-release
			return arr.CanaryPass
		},
		Notify: func(string, string) {},
	})
	entry := grabEntry(9)
	failed := grab.State{Key: strings.ToLower(entry.InfoHash), Phase: grab.PhaseFailed}
	m.failLater(grab.Grab{Entry: entry, State: failed}, grab.FailBadRelease)
	<-called
	close(release)
}
