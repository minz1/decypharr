package manager

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestRepairTableCooldownEndsAfterReinsertCooldown(t *testing.T) {
	t.Parallel()
	var table repairTable
	start := time.Now()
	table.fail("Key", "p", start)
	if !table.failed("key", "p", start.Add(reinsertCooldown-time.Nanosecond)) {
		t.Fatal("provider retried inside the cooldown")
	}
	if table.failed("key", "p", start.Add(reinsertCooldown)) {
		t.Fatal("provider still skipped once the cooldown ended")
	}
	if table.size() != 0 {
		t.Fatalf("table size = %d, want the expired mark dropped", table.size())
	}
}

func TestRepairTableKeepsOnlyKeysWithState(t *testing.T) {
	t.Parallel()
	var table repairTable
	first := &FixerRequest{done: make(chan struct{})}
	if got, owner := table.begin("Key", first); !owner || got != first {
		t.Fatalf("first begin = %p, %v; want the owner", got, owner)
	}
	if got, owner := table.begin("kEY", &FixerRequest{}); owner || got != first {
		t.Fatalf("second begin = %p, %v; want the first run and not the owner", got, owner)
	}
	if !table.running("KEY") {
		t.Fatal("running = false during a run")
	}
	table.fail("key", "p", time.Now())
	table.reset("key")
	if !table.running("key") {
		t.Fatal("reset ended the running repair")
	}
	table.end("key")
	if table.running("key") || table.size() != 0 {
		t.Fatalf("running = %v, size = %d; want an empty table after the run", table.running("key"), table.size())
	}
}

func TestFixTorrentSkipsAProviderThatRefusedInsideTheCooldown(t *testing.T) {
	t.Parallel()
	var submits atomic.Int64
	client := &healProvider{}
	client.submit = func(context.Context, *types.Torrent) (*types.Torrent, error) {
		submits.Add(1)
		return nil, errors.New("provider refused")
	}
	h := newHealHarness(t, client)
	for range 2 {
		if got := <-h.fixAsync(t.Context()); got.err == nil {
			t.Fatalf("heal = %#v; want a failure", got.result)
		}
	}
	if n := submits.Load(); n != 1 {
		t.Fatalf("submits = %d, want 1: the refusing provider is skipped inside the cooldown", n)
	}
	h.fixer.repairs.reset(h.entry.InfoHash)
	if got := <-h.fixAsync(t.Context()); got.err == nil {
		t.Fatalf("heal = %#v; want a failure", got.result)
	}
	if n := submits.Load(); n != 2 {
		t.Fatalf("submits = %d, want 2: a reset lets the provider be tried again", n)
	}
}

func TestFixTorrentRetriesAProviderOnceItsCooldownEnded(t *testing.T) {
	t.Parallel()
	h := newHealHarness(t, &healProvider{})
	h.fixer.repairs.fail(h.entry.InfoHash, "remaining", time.Now().Add(-reinsertCooldown-time.Minute))
	got := <-h.fixAsync(t.Context())
	if got.err != nil || got.result == nil || !got.result.Success {
		t.Fatalf("heal = %#v, error = %v; want success", got.result, got.err)
	}
	if n := h.fixer.repairs.size(); n != 0 {
		t.Fatalf("table size = %d, want 0 after a heal", n)
	}
}

func TestSwitchRefusedWhileARepairRuns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := newResultManager(t)
		storeResultEntry(t, m, func(e *storage.Entry) { e.Bad = false })
		stored, err := m.storage.Get(resultHash)
		if err != nil {
			t.Fatal(err)
		}
		m.clients.Store("remaining", &healProvider{})
		running := &FixerRequest{done: make(chan struct{})}
		m.fixer.repairs.begin(resultHash, running)
		job := &storage.SwitcherJob{
			ID: "job", InfoHash: resultHash, SourceProvider: "removed", TargetProvider: "remaining",
			Status: storage.SwitcherStatusPending,
		}
		m.executeMigration(t.Context(), job, stored)
		if job.Status != storage.SwitcherStatusFailed || job.Error != "repair in progress" {
			t.Fatalf("job = %+v, want failed with repair in progress", job)
		}
		if !m.fixer.repairs.running(resultHash) {
			t.Fatal("the refused switch ended the other repair")
		}
	})
}

func TestFixTorrentJoinsARunningSwitchAndGetsItsResult(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := newResultManager(t)
		storeResultEntry(t, m, func(e *storage.Entry) { e.Bad = false })
		stored, err := m.storage.Get(resultHash)
		if err != nil {
			t.Fatal(err)
		}
		release := make(chan struct{})
		client := &healProvider{}
		client.submit = func(ctx context.Context, torrent *types.Torrent) (*types.Torrent, error) {
			<-release
			return client.replacementProvider.SubmitMagnet(ctx, torrent)
		}
		m.clients.Store("remaining", client)
		job := &storage.SwitcherJob{
			ID: "job", InfoHash: resultHash, SourceProvider: "removed", TargetProvider: "remaining",
			Status: storage.SwitcherStatusPending,
		}
		go m.executeMigration(t.Context(), job, stored)
		synctest.Wait()
		type outcome struct {
			result *FixResult
			err    error
		}
		out := make(chan outcome, 1)
		go func() {
			result, fixErr := m.fixer.FixTorrent(t.Context(), stored, false)
			out <- outcome{result, fixErr}
		}()
		synctest.Wait()
		close(release)
		got := <-out
		if got.err != nil || got.result == nil || !got.result.Success {
			t.Fatalf("joined result = %#v, error = %v; want the switch's success", got.result, got.err)
		}
		if n := m.fixer.repairs.size(); n != 0 {
			t.Fatalf("table size = %d, want 0 after the switch", n)
		}
	})
}
