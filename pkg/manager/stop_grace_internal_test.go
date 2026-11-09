package manager

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// blockingDeleteProvider is a provider whose DeleteTorrent runs for hold, or until its ctx ends when hold is 0. Its
// GetTorrent still finds the torrent, so a cut delete is not mistaken for a gone one.
type blockingDeleteProvider struct {
	debrid.Client

	hold    time.Duration
	deletes atomic.Int32
	lookups atomic.Int32
}

func (p *blockingDeleteProvider) DeleteTorrent(ctx context.Context, _ string) error {
	p.deletes.Add(1)
	var finished <-chan time.Time
	if p.hold > 0 {
		timer := time.NewTimer(p.hold)
		defer timer.Stop()
		finished = timer.C
	}
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *blockingDeleteProvider) GetTorrent(ctx context.Context, id string) (*types.Torrent, error) {
	p.lookups.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &types.Torrent{ID: id}, nil
}

func (p *blockingDeleteProvider) calls() int32 { return p.deletes.Load() + p.lookups.Load() }

// newGraceHarness builds the prune harness with a lifecycle and the stop grace, inside the caller's synctest bubble,
// so the grace's timers run on the bubble's clock. Its torbox provider blocks deletes as given.
func newGraceHarness(t *testing.T, hold time.Duration) (*pruneHarness, *blockingDeleteProvider) {
	t.Helper()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.m.life = newLifecycle()
	h.m.initGrace(t.Context())
	t.Cleanup(h.m.graceCancel)
	provider := &blockingDeleteProvider{hold: hold}
	h.m.clients.Store("torbox", provider)
	return h, provider
}

func gracePlacement() *storage.ProviderEntry {
	return &storage.ProviderEntry{Provider: "torbox", ID: "id-cut", Progress: 1}
}

// startRelease runs releasePlacement on ctx in its own goroutine and hands back its error.
func startRelease(ctx context.Context, m *Manager) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := m.releasePlacement(ctx, "torbox", gracePlacement(), "self")
		done <- err
	}()
	return done
}

func requireRunning(t *testing.T, done <-chan error, when string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("the delete ended %s: %v", when, err)
	default:
	}
}

func requireEnded(t *testing.T, done <-chan error, when string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	default:
		t.Fatalf("the delete still runs %s", when)
		return nil
	}
}

// A delete started 10s before the stop keeps running past its own start+5s and is cut 5s after the stop.
func TestReleasePlacementStartedDeleteSurvivesUntilStopPlusGrace(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h, _ := newGraceHarness(t, 0)
		done := startRelease(t.Context(), h.m)

		time.Sleep(10 * time.Second)
		synctest.Wait()
		h.m.markStopping()
		time.Sleep(4 * time.Second)
		synctest.Wait()
		requireRunning(t, done, "4s after the stop")

		time.Sleep(time.Second)
		synctest.Wait()
		err := requireEnded(t, done, "5s after the stop")
		if !errors.Is(err, task.ErrGraceExpired) || !placementStopped(err) {
			t.Fatalf("cut delete err = %v, want ErrGraceExpired and a stopped placement", err)
		}
		if !strings.Contains(h.logs.String(), "may be left behind") {
			t.Fatalf("no left-behind WARN for the cut delete:\n%s", h.logs.String())
		}
	})
}

// With no stop, the grace never cuts a delete, however long the provider takes.
func TestReleasePlacementNormalDeleteNotCut(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h, provider := newGraceHarness(t, time.Hour)
		started := time.Now()
		_, err := h.m.releasePlacement(t.Context(), "torbox", gracePlacement(), "self")
		if err != nil {
			t.Fatalf("a slow delete in normal running failed: %v", err)
		}
		if got := time.Since(started); got != time.Hour {
			t.Fatalf("the delete returned after %v, want the provider's 1h", got)
		}
		if provider.deletes.Load() != 1 {
			t.Fatalf("deletes = %d, want 1", provider.deletes.Load())
		}
	})
}

// A done owner ctx, or a grace that has run out, refuses the release before any provider call.
func TestReleasePlacementDoneCtxNoProviderCall(t *testing.T) {
	t.Parallel()
	t.Run("owner ctx done", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			h, provider := newGraceHarness(t, 0)
			owner, cancel := context.WithCancelCause(t.Context())
			cancel(task.ErrStopped)
			_, err := h.m.releasePlacement(owner, "torbox", gracePlacement(), "self")
			if !placementStopped(err) || !errors.Is(err, task.ErrStopped) {
				t.Fatalf("err = %v, want a stopped placement naming the owner's cause", err)
			}
			if provider.calls() != 0 {
				t.Fatalf("provider calls = %d, want 0", provider.calls())
			}
		})
	})
	t.Run("grace expired", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			h, provider := newGraceHarness(t, 0)
			h.m.markStopping()
			time.Sleep(placementStopGrace)
			synctest.Wait()
			_, err := h.m.releasePlacement(t.Context(), "torbox", gracePlacement(), "self")
			if !placementStopped(err) || !errors.Is(err, task.ErrGraceExpired) {
				t.Fatalf("err = %v, want a stopped placement with ErrGraceExpired", err)
			}
			if provider.calls() != 0 {
				t.Fatalf("provider calls = %d, want 0", provider.calls())
			}
		})
	})
}

// A work job blocked in a provider delete does not hold the work group's stop past the grace, while the placements
// group, which stops later, is still live.
func TestWorkStopBoundedByDeleteGrace(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h, provider := newGraceHarness(t, 0)
		if err := h.m.work.Go("job", func(ctx context.Context) error {
			_, err := h.m.releasePlacement(ctx, "torbox", gracePlacement(), "self")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if provider.deletes.Load() != 1 {
			t.Fatalf("deletes = %d, want the job's delete in flight", provider.deletes.Load())
		}

		h.m.markStopping()
		stopped := make(chan time.Duration, 1)
		go func() {
			started := time.Now()
			ctx, cancel := task.Detached(t.Context(), time.Minute)
			defer cancel()
			_ = h.m.work.Stop(ctx)
			stopped <- time.Since(started)
		}()
		time.Sleep(placementStopGrace)
		synctest.Wait()
		select {
		case took := <-stopped:
			if took > placementStopGrace {
				t.Fatalf("the work group stopped after %v, want within %v of the stop", took, placementStopGrace)
			}
		default:
			t.Fatalf("the work group still waits on the delete %v after the stop", placementStopGrace)
		}
		if err := h.m.placementsSvc.group.Context().Err(); err != nil {
			t.Fatalf("the placements group ended (%v); the bound must come from the grace", err)
		}
	})
}

// One shared grace: a delete started 4s after the stop still runs, and is cut at 5s, not 9s.
func TestDeleteStartedAfterStopCutAtSharedGrace(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h, provider := newGraceHarness(t, 0)
		h.m.markStopping()
		time.Sleep(4 * time.Second)
		done := startRelease(t.Context(), h.m)
		synctest.Wait()
		requireRunning(t, done, "4s after the stop")
		if provider.deletes.Load() != 1 {
			t.Fatalf("deletes = %d, want the late delete started", provider.deletes.Load())
		}

		time.Sleep(time.Second)
		synctest.Wait()
		if err := requireEnded(t, done, "5s after the stop"); !errors.Is(err, task.ErrGraceExpired) {
			t.Fatalf("late delete err = %v, want ErrGraceExpired", err)
		}
	})
}

// A delete queued on the provider lock while the grace runs out makes no provider call once it gets the lock. This
// runs on the real clock: synctest does not see a goroutine waiting on a mutex as blocked. The grace is 0, so it
// ends as soon as the stop begins.
func TestDeleteQueuedOnLockAfterGraceMakesNoCall(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.m.life = newLifecycle()
	h.m.stopping, h.m.stopCause = context.WithCancelCause(context.WithoutCancel(t.Context()))
	h.m.grace, h.m.graceCancel = task.Grace(h.m.stopping, 0)
	t.Cleanup(h.m.graceCancel)
	provider := &blockingDeleteProvider{}
	h.m.clients.Store("torbox", provider)

	lock := h.m.providerLock("torbox")
	lock.Lock()
	done := startRelease(t.Context(), h.m)
	for range 1000 {
		runtime.Gosched() // let the release reach the lock
	}
	h.m.markStopping()
	<-h.m.grace.Done()
	lock.Unlock()

	if err := <-done; !errors.Is(err, task.ErrGraceExpired) || !placementStopped(err) {
		t.Fatalf("queued delete err = %v, want ErrGraceExpired", err)
	}
	if provider.calls() != 0 {
		t.Fatalf("provider calls = %d, want 0", provider.calls())
	}
}

// markStopping ends stopping on its first call, and also when the lifecycle was already Stopping.
func TestMarkStoppingCancelsStoppingAlways(t *testing.T) {
	t.Parallel()
	for _, already := range []bool{false, true} {
		h := newPruneHarness(t, config.PruneModeDelete)
		h.m.life = newLifecycle()
		h.m.initGrace(t.Context())
		t.Cleanup(h.m.graceCancel)
		if already && !h.m.life.Transition(h.m.life.Load(), lifecycleStopping) {
			t.Fatal("could not move the lifecycle to Stopping")
		}
		h.m.markStopping()
		if !errors.Is(context.Cause(h.m.stopping), task.ErrStopped) {
			t.Fatalf("already Stopping=%v: stopping cause = %v, want ErrStopped", already, context.Cause(h.m.stopping))
		}
	}
}

// After closeStorage released the grace, a release still reads as a stop and calls nothing.
func TestRefusalAfterGraceCancelReadsStopped(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h, provider := newGraceHarness(t, 0)
		h.m.markStopping()
		h.m.graceCancel()
		_, err := h.m.releasePlacement(t.Context(), "torbox", gracePlacement(), "self")
		if !errors.Is(err, task.ErrStopped) || !placementStopped(err) {
			t.Fatalf("err = %v, want ErrStopped", err)
		}
		if provider.calls() != 0 {
			t.Fatalf("provider calls = %d, want 0", provider.calls())
		}
	})
}

// RemoveTorrentPlacements runs on the placements group, not a request ctx: a slow delete finishes, and once the
// group has closed a refusal is logged as left behind, not as a provider failure.
func TestRemoveTorrentPlacementsIgnoresRequestCancel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h, provider := newGraceHarness(t, time.Minute)
		entry := &storage.Entry{
			InfoHash:  "gone",
			Providers: map[string]*storage.ProviderEntry{"torbox": gracePlacement()},
		}
		h.m.RemoveTorrentPlacements(entry)
		if provider.deletes.Load() != 1 || provider.lookups.Load() != 0 {
			t.Fatalf(
				"deletes=%d lookups=%d, want one finished delete",
				provider.deletes.Load(),
				provider.lookups.Load(),
			)
		}

		closePlacements(t, h.m)
		h.m.RemoveTorrentPlacements(entry)
		if provider.deletes.Load() != 1 {
			t.Fatalf("deletes = %d after the group closed, want no new call", provider.deletes.Load())
		}
		logs := h.logs.String()
		if !strings.Contains(logs, "left behind (stopping)") || strings.Contains(logs, "Failed to delete") {
			t.Fatalf("a refused delete was not logged as left behind:\n%s", logs)
		}
	})
}

// A prune batch that the stop refuses ends quietly: nothing deleted, nothing counted as failed, entries kept.
func TestPruneEndsQuietlyWhenStopRefusesRemoval(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h, provider := newGraceHarness(t, 0)
		h.addEntry(t, "aaaa", nil)
		h.addEntry(t, "bbbb", nil)
		cs := h.cands(t, "aaaa", "bbbb")
		h.m.markStopping()
		h.m.graceCancel()

		deleted, failed, _ := h.m.pruneEntries(t.Context(), cs)
		if deleted != 0 || failed != 0 || provider.calls() != 0 {
			t.Fatalf("deleted=%d failed=%d provider calls=%d, want all zero", deleted, failed, provider.calls())
		}
		if !h.has("aaaa") || !h.has("bbbb") || len(h.m.pruneFails) != 0 {
			t.Fatalf("entries kept aaaa=%v bbbb=%v, fails=%v", h.has("aaaa"), h.has("bbbb"), h.m.pruneFails)
		}
		logs := h.logs.String()
		if strings.Count(logs, "left behind (stopping)") != 1 || strings.Contains(logs, "provider removal failed") {
			t.Fatalf("want one quiet left-behind line and no failure:\n%s", logs)
		}
	})
}
