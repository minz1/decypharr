package manager

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/storage"
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
