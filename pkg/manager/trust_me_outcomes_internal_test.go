package manager

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/rs/zerolog"

	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// SM12-23: item 62's accepted trust-me outcomes, one test each, named for the outcome.

// verdictAfterStopProvider answers the status check with a dead-release verdict, but only after the stop has landed.
type verdictAfterStopProvider struct {
	pendingStatusProvider

	stop context.CancelFunc
}

func (p *verdictAfterStopProvider) CheckStatus(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	p.stop()
	return &types.Torrent{ID: t.ID, Name: t.Name, Status: types.TorrentStatusError}, nil
}

// (a) A stop that lands after CheckStatus returned a verdict discards the verdict: the entry stays queued as
// downloading and the provider's torrent is kept (5fbadfdd).
func TestQueuedStopAfterStatusVerdictKeepsEntryAndTorrent(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeArrRecovery{}
		ctx, stop := context.WithCancel(t.Context())
		defer stop()
		provider := &verdictAfterStopProvider{stop: stop}
		m, entry := pendingEntryManager(t, fake, &provider.pendingStatusProvider)
		m.clients.Store("provider", provider)

		m.processQueuedTorrent(ctx, entry)
		drainPlacements(t, m)

		saved, err := m.queue.GetTorrent(entry.InfoHash)
		if err != nil {
			t.Fatal(err)
		}
		if saved.State != storage.EntryStateDownloading || saved.LastError != "" {
			t.Fatalf("state = %q, last error %q; want the entry left downloading", saved.State, saved.LastError)
		}
		if len(fake.fails) != 0 || provider.deleted.Load() != 0 {
			t.Fatalf("FailDownload calls = %v, provider deletes = %d; want none", fake.fails, provider.deleted.Load())
		}
	})
}

// (b) A heal whose replacement comes back with no files, with the stop landing before the discard, leaves the
// replacement at the provider and logs it at INFO as left behind, not as a failed delete (b5aea6e3).
func TestHealZeroFilesStopBeforeDiscardLeavesReplacement(t *testing.T) {
	t.Parallel()
	client := &healProvider{}
	h := newHealHarness(t, client)
	var logs bytes.Buffer
	h.fixer.manager.logger = zerolog.New(&logs)
	client.check = func(_ context.Context, torrent *types.Torrent) (*types.Torrent, error) {
		h.cancelWork()
		torrent.Files = nil
		return torrent, nil
	}
	handed, cancel := context.WithCancel(h.fixer.manager.work.Context())
	defer cancel()
	got := <-h.fixAsync(handed)
	if got.err == nil || (got.result != nil && got.result.Success) {
		t.Fatalf("heal = %#v, error = %v; want a stopped heal", got.result, got.err)
	}
	if n := client.deletions.Load(); n != 0 {
		t.Fatalf("deletes = %d, want the replacement left at the provider", n)
	}
	if n := h.confirms.Load(); n != 0 {
		t.Fatalf("give-up checks = %d, want 0", n)
	}
	out := logs.String()
	if !strings.Contains(out, "Provider torrent left behind (stopping)") || strings.Contains(out, "Failed to delete") {
		t.Fatalf("want one INFO left-behind line and no failed delete:\n%s", out)
	}
}

// failThenHangProvider fails every delete for a real reason; its GetTorrent check then holds until its ctx ends.
type failThenHangProvider struct {
	debrid.Client

	deletes atomic.Int32
	looking chan struct{}
}

func (p *failThenHangProvider) DeleteTorrent(context.Context, string) error {
	p.deletes.Add(1)
	return errors.New("provider answered 500")
}

func (p *failThenHangProvider) GetTorrent(ctx context.Context, _ string) (*types.Torrent, error) {
	close(p.looking)
	<-ctx.Done()
	return nil, ctx.Err()
}

// (c) A delete that fails for a real reason, whose grace runs out during the GetTorrent check, is classified as
// stopped: the prune keeps the entry and counts no failure (9ebbb178).
func TestPruneDeleteFailedGraceEndsDuringLookupNoFailCount(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h, _ := newGraceHarness(t, 0)
		provider := &failThenHangProvider{looking: make(chan struct{})}
		h.m.clients.Store("torbox", provider)
		h.addEntry(t, "aaaa", nil)
		cs := h.cands(t, "aaaa")

		type counts struct{ deleted, failed int }
		done := make(chan counts, 1)
		go func() {
			deleted, failed, _ := h.m.pruneEntries(t.Context(), cs)
			done <- counts{deleted, failed}
		}()
		<-provider.looking
		h.m.markStopping()
		got := <-done

		if got.deleted != 0 || got.failed != 0 || provider.deletes.Load() != 1 {
			t.Fatalf("deleted=%d failed=%d deletes=%d, want 0, 0, 1", got.deleted, got.failed, provider.deletes.Load())
		}
		if !h.has("aaaa") || len(h.m.pruneFails) != 0 {
			t.Fatalf("entry kept = %v, prune fails = %v; want kept and no count", h.has("aaaa"), h.m.pruneFails)
		}
		logs := h.logs.String()
		if !strings.Contains(logs, "left behind (stopping)") || strings.Contains(logs, "provider removal failed") {
			t.Fatalf("want a quiet left-behind line and no failure:\n%s", logs)
		}
	})
}
