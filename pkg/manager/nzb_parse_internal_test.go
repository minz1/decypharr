package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/hearsay"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
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
		work:          testGroup(t),
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

func storedEntry(t *testing.T, m *Manager) *storage.Entry {
	t.Helper()
	entry, err := m.queue.GetTorrent("nzb-1")
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// newParseChildCall stages nzb-1 and returns the call of its submit child on a manager whose parse is parse.
func newParseChildCall(t *testing.T, parse parseFunc) (*Manager, entryChildCall) {
	t.Helper()
	m := withTestConfig(t, newParseRetryManager(t, parse))
	m.usenet = newUnreachableUsenet(t)
	staged := filepath.Join(t.TempDir(), "Show.S01.nzb")
	if err := os.WriteFile(staged, []byte("<nzb/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := storedEntry(t, m)
	entry.Magnet = staged
	entry.Category = "sonarr"
	call := entryChildCall{
		kind: grab.ChildSubmit,
		grab: grab.Grab{State: grab.State{Key: "nzb-1", Phase: grab.PhaseSubmitting, NZB: true}, Entry: entry},
	}
	return m, call
}

func parsedGroups() map[string]*parser.FileGroup {
	return map[string]*parser.FileGroup{"a": {
		BaseName: "a",
		Files:    []manifest.File{{Segments: []manifest.Segment{{Number: 1, MessageID: "a@example"}}}},
	}}
}

func assertParseFailed(t *testing.T, msg entryMsg, reason string) {
	t.Helper()
	if msg.ev == nil || msg.ev.Kind != grab.EvParseResult || msg.ev.Outcome != grab.OutcomePermanent ||
		msg.ev.Fail != grab.FailEntry || !strings.Contains(msg.ev.Reason, reason) {
		t.Fatalf("parse result = %+v, want a permanent failure mentioning %q", msg.ev, reason)
	}
}

func TestParseChildThrottlesAParseThatRanOutOfTime(t *testing.T) {
	t.Parallel()
	m, call := newParseChildCall(
		t,
		func(ctx context.Context, _, _ string, _ []byte, _ string) (*storage.NZB, map[string]*parser.FileGroup, error) {
			return nil, nil, waitOutBudget(ctx)
		},
	)

	msg := m.parseChild(t.Context(), call)

	if msg.ev == nil || msg.ev.Kind != grab.EvParseResult || msg.ev.Outcome != grab.OutcomeThrottled {
		t.Fatalf("parse result = %+v, want a throttled outcome", msg.ev)
	}
}

func TestParseChildFailsAtOnceOnAMissingSegment(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	m, call := newParseChildCall(
		t,
		func(context.Context, string, string, []byte, string) (*storage.NZB, map[string]*parser.FileGroup, error) {
			calls.Add(1)
			return nil, nil, customerror.ErrUsenetSegmentMissing
		},
	)

	assertParseFailed(t, m.parseChild(t.Context(), call), "usenet parse failed")
	if calls.Load() != 1 {
		t.Fatalf("parsed %d times, want 1", calls.Load())
	}
}

func TestParseChildReportsNothingOnceStopped(t *testing.T) {
	t.Parallel()
	m, call := newParseChildCall(
		t,
		func(context.Context, string, string, []byte, string) (*storage.NZB, map[string]*parser.FileGroup, error) {
			return &storage.NZB{ID: "nzb-1", Name: "Show.S01"}, parsedGroups(), nil
		},
	)
	m.usenetTimeout = time.Minute
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if msg := m.parseChild(ctx, call); msg.ev != nil {
		t.Fatalf("parse result = %+v, want none so the grab is looked up", msg.ev)
	}
	if _, err := os.Stat(call.grab.Entry.Magnet); err != nil {
		t.Fatalf("staged NZB lost by the interrupted parse: %v", err)
	}
}

func TestParseChildFailsAProcessRunThatOutlivesItsBudget(t *testing.T) {
	t.Parallel()
	m, call := newParseChildCall(
		t,
		func(context.Context, string, string, []byte, string) (*storage.NZB, map[string]*parser.FileGroup, error) {
			return &storage.NZB{ID: "nzb-1", Name: "Show.S01"}, parsedGroups(), nil
		},
	)
	m.usenetTimeout = time.Nanosecond

	assertParseFailed(t, m.parseChild(t.Context(), call), "failed to process nzb")
}

// Hearsay's verdict that the segments are gone is final: the grab fails at once instead of parsing again.
func TestParseChildFailsOnHearsayIncompleteVerdict(t *testing.T) {
	t.Parallel()
	groups := parsedGroups()
	var calls atomic.Int32
	m, call := newParseChildCall(
		t,
		func(context.Context, string, string, []byte, string) (*storage.NZB, map[string]*parser.FileGroup, error) {
			calls.Add(1)
			return &storage.NZB{Name: "Show.S01"}, groups, nil
		},
	)
	cfg := &config.Config{}
	cfg.WithDir(t.TempDir())
	cfg.Hearsay.AdviceMode = "active"
	cfg.Usenet.Providers = []config.UsenetProvider{{Host: "news.example", Backbone: "omicron"}}
	service, err := hearsay.New(testGroup(t), cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	service.ReportNZB(hearsay.NZBSubjectFromGroups(groups), false)
	m.hearsay = service

	assertParseFailed(t, m.parseChild(t.Context(), call), "hearsay")
	if calls.Load() != 1 {
		t.Fatalf("parsed %d times, want 1", calls.Load())
	}
}
