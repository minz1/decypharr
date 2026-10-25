package manager

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/kvstore"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/hearsay"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/manifest"
	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
)

type parseFunc = func(ctx context.Context, id, name string, content []byte, category string) (*storage.NZB, map[string]*parser.FileGroup, error)

func newParseRetryManager(t *testing.T, parse parseFunc) *Manager {
	t.Helper()
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := &Manager{
		queue:         newQueue(store, "", nil, zerolog.Nop()),
		logger:        zerolog.Nop(),
		usenetTimeout: 20 * time.Millisecond,
		parseNZBFunc:  parse,
	}
	if addErr := m.queue.Add(
		&storage.Entry{InfoHash: "nzb-1", Name: "Show.S01", Protocol: config.ProtocolNZB},
	); addErr != nil {
		t.Fatal(addErr)
	}
	return m
}

func waitOutBudget(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// Obfuscated multi-episode batches share the article broker and can each run
// out of parse budget while the provider is healthy; one more budget usually
// finishes from the broker's cache instead of blocklisting a good release.
func TestParseNZBRetriesOnceWhenOnlyItsBudgetRanOut(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	m := newParseRetryManager(
		t,
		func(ctx context.Context, _, _ string, _ []byte, _ string) (*storage.NZB, map[string]*parser.FileGroup, error) {
			if calls.Add(1) == 1 {
				return nil, nil, waitOutBudget(ctx)
			}
			return &storage.NZB{Name: "Show.S01"}, nil, nil
		},
	)

	meta, _, err := m.parseNZB(t.Context(), "nzb-1", "Show.S01", nil, "sonarr")
	if err != nil || meta == nil || calls.Load() != 2 {
		t.Fatalf("parse = %v, %v after %d calls, want the second attempt's result", meta, err, calls.Load())
	}
}

func TestParseNZBGivesUpAfterTwoBudgets(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	m := newParseRetryManager(
		t,
		func(ctx context.Context, _, _ string, _ []byte, _ string) (*storage.NZB, map[string]*parser.FileGroup, error) {
			calls.Add(1)
			return nil, nil, waitOutBudget(ctx)
		},
	)

	_, _, err := m.parseNZB(t.Context(), "nzb-1", "Show.S01", nil, "sonarr")
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 2 {
		t.Fatalf("parse err = %v after %d calls, want the deadline after 2", err, calls.Load())
	}
}

func TestParseNZBDoesNotRetry(t *testing.T) {
	t.Parallel()
	cases := map[string]func(t *testing.T, m *Manager) (context.Context, parseFunc){
		"missing segment": func(t *testing.T, _ *Manager) (context.Context, parseFunc) {
			return t.Context(), func(context.Context, string, string, []byte, string) (*storage.NZB, map[string]*parser.FileGroup, error) {
				return nil, nil, customerror.ErrUsenetSegmentMissing
			}
		},
		"missing segment as the budget ran out": func(t *testing.T, _ *Manager) (context.Context, parseFunc) {
			return t.Context(), func(ctx context.Context, _, _ string, _ []byte, _ string) (*storage.NZB, map[string]*parser.FileGroup, error) {
				_ = waitOutBudget(ctx)
				return nil, nil, customerror.ErrUsenetSegmentMissing
			}
		},
		"an operation's own deadline": func(t *testing.T, _ *Manager) (context.Context, parseFunc) {
			return t.Context(), func(context.Context, string, string, []byte, string) (*storage.NZB, map[string]*parser.FileGroup, error) {
				return nil, nil, context.DeadlineExceeded
			}
		},
		"caller cancelled": func(t *testing.T, _ *Manager) (context.Context, parseFunc) {
			ctx, cancel := context.WithCancel(t.Context())
			return ctx, func(ctx context.Context, _, _ string, _ []byte, _ string) (*storage.NZB, map[string]*parser.FileGroup, error) {
				cancel()
				return nil, nil, ctx.Err()
			}
		},
		"caller deadline": func(t *testing.T, _ *Manager) (context.Context, parseFunc) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			t.Cleanup(cancel)
			return ctx, func(ctx context.Context, _, _ string, _ []byte, _ string) (*storage.NZB, map[string]*parser.FileGroup, error) {
				return nil, nil, waitOutBudget(ctx)
			}
		},
		"entry removed": func(t *testing.T, m *Manager) (context.Context, parseFunc) {
			return t.Context(), func(ctx context.Context, id, _ string, _ []byte, _ string) (*storage.NZB, map[string]*parser.FileGroup, error) {
				if err := m.queue.Delete(id, false, nil); err != nil {
					t.Error(err)
				}
				return nil, nil, waitOutBudget(ctx)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newParseRetryManager(t, nil)
			ctx, parse := setup(t, m)
			var calls atomic.Int32
			m.parseNZBFunc = func(ctx context.Context, id, n string, c []byte, cat string) (*storage.NZB, map[string]*parser.FileGroup, error) {
				calls.Add(1)
				return parse(ctx, id, n, c, cat)
			}

			if _, _, err := m.parseNZB(ctx, "nzb-1", "Show.S01", nil, "sonarr"); err == nil || calls.Load() != 1 {
				t.Fatalf("parse err = %v after %d calls, want the first failure and no retry", err, calls.Load())
			}
		})
	}
}

// The failure line is the one an operator greps for; it must say which NZB failed.
func TestActiveDownloadFailureLogNamesTheEntry(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	m := newParseRetryManager(t, nil)
	m = withTestConfig(t, m)
	m.logger = zerolog.New(&logs).Level(zerolog.InfoLevel)
	entry, err := m.queue.GetTorrent("nzb-1")
	if err != nil {
		t.Fatal(err)
	}

	m.handleJobError(&Job{ID: "nzb-1", Type: JobTypeNZB, Entry: entry}, errors.New("usenet parse failed"))

	if !strings.Contains(logs.String(), `"name":"Show.S01"`) {
		t.Fatalf("failure log = %s, want the entry name", logs.String())
	}
}

// A delete that lands while the job runs must not come back as a failed record the arr already removed.
func TestJobErrorForADeletedEntryWritesNothingBack(t *testing.T) {
	t.Parallel()
	m := withTestConfig(t, newParseRetryManager(t, nil))
	entry, err := m.queue.GetTorrent("nzb-1")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.queue.Delete("nzb-1", false, nil); err != nil {
		t.Fatal(err)
	}

	m.handleJobError(&Job{ID: "nzb-1", Type: JobTypeNZB, Entry: entry}, errors.New("usenet parse failed"))

	if m.queue.Contains("nzb-1") {
		t.Fatal("the failure re-created the deleted entry")
	}
}

type roundParse = func(ctx context.Context, call int32) (*storage.NZB, error)

// newParseQueueManager queues nzb-1 as a staged NZB on a real job queue; calls gets each parse call's number as it returns.
func newParseQueueManager(t *testing.T, delay time.Duration, parse roundParse) (*Manager, <-chan int32) {
	t.Helper()
	ended := make(chan int32, 16)
	var n atomic.Int32
	m := withTestConfig(t, newParseRetryManager(
		t,
		func(ctx context.Context, _, _ string, _ []byte, _ string) (*storage.NZB, map[string]*parser.FileGroup, error) {
			call := n.Add(1)
			meta, err := parse(ctx, call)
			select {
			case ended <- call:
			default: // a runaway requeue must not block the worker past Close
			}
			return meta, nil, err
		},
	))
	m.parseRequeueDelay = delay
	m.usenet = newUnreachableUsenet(t)
	staged := filepath.Join(t.TempDir(), "Show.S01.nzb")
	if err := os.WriteFile(staged, []byte("<nzb/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err := m.queue.GetTorrent("nzb-1")
	if err != nil {
		t.Fatal(err)
	}
	entry.Magnet = staged
	entry.SavePath = t.TempDir()
	entry.Category = "sonarr"
	entry.Status = debridTypes.TorrentStatusQueued
	if err = m.queue.Update(entry); err != nil {
		t.Fatal(err)
	}
	m.jobQueue = NewJobQueue(t.Context(), 1, m.processJob, zerolog.Nop())
	t.Cleanup(m.jobQueue.Close)
	job := NewJob(JobTypeNZB, &ImportRequest{ID: "nzb-1", Name: "Show.S01", Arr: m.arr.GetOrCreate("sonarr")})
	job.Entry = entry
	if err = m.jobQueue.Submit(job); err != nil {
		t.Fatal(err)
	}
	return m, ended
}

func awaitCall(t *testing.T, calls <-chan int32, want int32) {
	t.Helper()
	for {
		select {
		case got := <-calls:
			if got == want {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("parse call %d never came", want)
		}
	}
}

func noCallWithin(t *testing.T, calls <-chan int32, d time.Duration) {
	t.Helper()
	select {
	case got := <-calls:
		t.Fatalf("unexpected parse call %d", got)
	case <-time.After(d):
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func storedEntry(t *testing.T, m *Manager) *storage.Entry {
	t.Helper()
	entry, err := m.queue.GetTorrent("nzb-1")
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// A parse that keeps running out of time costs the grab only after three rounds, so a busy broker doesn't blocklist a good release.
func TestParseRequeuedTwiceThenFailsTheGrab(t *testing.T) {
	t.Parallel()
	m, calls := newParseQueueManager(t, 20*time.Millisecond, func(ctx context.Context, _ int32) (*storage.NZB, error) {
		return nil, waitOutBudget(ctx)
	})

	awaitCall(t, calls, 4)
	eventually(t, "the second backoff", func() bool { return waitingJobs(m.jobQueue) == 1 })
	if entry := storedEntry(t, m); entry.State == storage.EntryStateError {
		t.Fatalf("entry failed after two rounds: %s", entry.LastError)
	}
	awaitCall(t, calls, 6)
	eventually(t, "the grab to fail", func() bool { return storedEntry(t, m).State == storage.EntryStateError })
	if entry := storedEntry(t, m); !strings.Contains(entry.LastError, "gave up after 3 parse rounds") {
		t.Fatalf("last error = %q, want the give-up", entry.LastError)
	}
	noCallWithin(t, calls, 100*time.Millisecond)
}

func TestParseSucceedsOnTheSecondRequeue(t *testing.T) {
	t.Parallel()
	m, calls := newParseQueueManager(
		t,
		20*time.Millisecond,
		func(ctx context.Context, call int32) (*storage.NZB, error) {
			if call < 5 {
				return nil, waitOutBudget(ctx)
			}
			return &storage.NZB{Name: "Show.S01"}, nil
		},
	)

	awaitCall(t, calls, 5)
	eventually(t, "the job to finish", func() bool { return !m.jobQueue.Holds("nzb-1") })
	entry := storedEntry(t, m)
	if entry.ActiveProvider != usenetProvider || !strings.HasPrefix(entry.LastError, "failed to process nzb") {
		t.Fatalf(
			"entry provider %q, last error %q, want a parsed NZB past the parse",
			entry.ActiveProvider,
			entry.LastError,
		)
	}
}

func TestParseSegmentMissingFailsAtOnce(t *testing.T) {
	t.Parallel()
	m, calls := newParseQueueManager(t, 20*time.Millisecond, func(context.Context, int32) (*storage.NZB, error) {
		return nil, customerror.ErrUsenetSegmentMissing
	})

	awaitCall(t, calls, 1)
	eventually(t, "the grab to fail", func() bool { return storedEntry(t, m).State == storage.EntryStateError })
	noCallWithin(t, calls, 100*time.Millisecond)
}

func TestDeleteDuringParseBackoffLeavesNothing(t *testing.T) {
	t.Parallel()
	m, calls := newParseQueueManager(t, 300*time.Millisecond, func(ctx context.Context, _ int32) (*storage.NZB, error) {
		return nil, waitOutBudget(ctx)
	})

	awaitCall(t, calls, 2)
	eventually(t, "the backoff", func() bool { return waitingJobs(m.jobQueue) == 1 })
	if err := m.queue.Delete("nzb-1", true, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the job to let go", func() bool { return !m.jobQueue.Holds("nzb-1") })
	noCallWithin(t, calls, 50*time.Millisecond)
	if m.queue.Contains("nzb-1") {
		t.Fatal("the deleted entry came back")
	}
}

func TestShutdownDuringParseBackoffKeepsTheEntry(t *testing.T) {
	t.Parallel()
	m, calls := newParseQueueManager(t, time.Hour, func(ctx context.Context, _ int32) (*storage.NZB, error) {
		return nil, waitOutBudget(ctx)
	})

	awaitCall(t, calls, 2)
	eventually(t, "the backoff to hold the entry", func() bool {
		return waitingJobs(m.jobQueue) == 1 && m.jobQueue.Holds("nzb-1")
	})
	m.jobQueue.Close()
	eventually(t, "the backoff to stop", func() bool { return !m.jobQueue.Holds("nzb-1") })
	if entry := storedEntry(
		t,
		m,
	); entry.Status != debridTypes.TorrentStatusQueued ||
		entry.State == storage.EntryStateError {
		t.Fatalf("entry status %q state %q, want it left queued for the next start", entry.Status, entry.State)
	}
}

func TestDeleteDuringParseRoundLeavesNothing(t *testing.T) {
	t.Parallel()
	var m *Manager
	ready := make(chan struct{})
	m, calls := newParseQueueManager(t, 20*time.Millisecond, func(ctx context.Context, _ int32) (*storage.NZB, error) {
		<-ready
		if err := m.queue.Delete("nzb-1", false, nil); err != nil {
			t.Error(err)
		}
		return nil, waitOutBudget(ctx)
	})
	close(ready)

	awaitCall(t, calls, 1)
	eventually(t, "the job to let go", func() bool { return !m.jobQueue.Holds("nzb-1") })
	noCallWithin(t, calls, 50*time.Millisecond)
	if m.queue.Contains("nzb-1") {
		t.Fatal("the failure re-created the deleted entry")
	}
}

// Only a missing record means the arr deleted the entry; a record that fails to read still gets the job's failure.
func TestJobErrorForAnUnreadableEntryStillFailsIt(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "db")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := kvstore.Open(filepath.Join(dir, "queue.db"), appendstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = raw.Put("nzb-1", []byte{0xff}, nil); err != nil {
		t.Fatal(err)
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewStorage(dir, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := withTestConfig(t, &Manager{queue: newQueue(store, "", nil, zerolog.Nop()), logger: zerolog.Nop()})
	entry := &storage.Entry{InfoHash: "nzb-1", Name: "Show.S01", Protocol: config.ProtocolNZB}

	m.handleJobError(&Job{ID: "nzb-1", Type: JobTypeNZB, Entry: entry}, errors.New("usenet parse failed"))

	if saved, getErr := m.queue.GetTorrent("nzb-1"); getErr != nil || saved.State != storage.EntryStateError {
		t.Fatalf("entry = %+v, %v: the job failure was dropped", saved, getErr)
	}
}

// Hearsay's verdict that the segments are gone is final: the entry fails at once instead of parsing again.
func TestHearsayIncompleteNZBFailsAtOnce(t *testing.T) {
	t.Parallel()
	groups := map[string]*parser.FileGroup{"a": {
		BaseName: "a",
		Files:    []manifest.File{{Segments: []manifest.Segment{{Number: 1, MessageID: "a@example"}}}},
	}}
	var calls atomic.Int32
	m := withTestConfig(t, newParseRetryManager(
		t,
		func(context.Context, string, string, []byte, string) (*storage.NZB, map[string]*parser.FileGroup, error) {
			calls.Add(1)
			return &storage.NZB{Name: "Show.S01"}, groups, nil
		},
	))
	cfg := &config.Config{}
	cfg.WithDir(t.TempDir())
	cfg.Hearsay.AdviceMode = "active"
	cfg.Usenet.Providers = []config.UsenetProvider{{Host: "news.example", Backbone: "omicron"}}
	service, err := hearsay.New(cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	service.ReportNZB(hearsay.NZBSubjectFromGroups(groups), false)
	m.hearsay = service
	m.usenet = newUnreachableUsenet(t)
	m.parseRequeueDelay = time.Hour
	m.jobQueue = NewJobQueue(t.Context(), 1, m.processJob, zerolog.Nop())
	t.Cleanup(m.jobQueue.Close)
	staged := filepath.Join(t.TempDir(), "Show.S01.nzb")
	if err = os.WriteFile(staged, []byte("<nzb/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := storedEntry(t, m)
	entry.Magnet = staged
	if err = m.queue.Update(entry); err != nil {
		t.Fatal(err)
	}
	job := NewJob(JobTypeNZB, &ImportRequest{ID: "nzb-1", Name: "Show.S01", Arr: m.arr.GetOrCreate("sonarr")})
	job.Entry = entry

	m.processJob(t.Context(), job)

	saved := storedEntry(t, m)
	if saved.State != storage.EntryStateError || !strings.Contains(saved.LastError, "hearsay") {
		t.Fatalf("entry state %q, last error %q, want it failed on hearsay's verdict", saved.State, saved.LastError)
	}
	if calls.Load() != 1 || waitingJobs(m.jobQueue) != 0 || m.jobQueue.Holds("nzb-1") {
		t.Fatalf(
			"parsed %d times, %d waiting: a hearsay-incomplete NZB was requeued",
			calls.Load(),
			waitingJobs(m.jobQueue),
		)
	}
}
