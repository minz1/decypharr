package manager

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
)

func TestTorrentSubmissionGateUsesSlidingWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	gate := newTorrentSubmissionGate(5 * time.Minute)
	gate.clock = func() time.Time { return now }

	var calls int
	submit := func() error {
		calls++
		return nil
	}

	if err := gate.Do(t.Context(), "torbox:hash", submit); err != nil {
		t.Fatal(err)
	}
	now = now.Add(4 * time.Minute)
	if err := gate.Do(t.Context(), "torbox:hash", submit); err != nil {
		t.Fatal(err)
	}
	now = now.Add(4 * time.Minute)
	if err := gate.Do(t.Context(), "torbox:hash", submit); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("submit calls = %d, want 1 while duplicate requests keep the window active", calls)
	}

	now = now.Add(5 * time.Minute)
	if err := gate.Do(t.Context(), "torbox:hash", submit); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("submit calls = %d, want 2 after a quiet window", calls)
	}
}

func TestTorrentSubmissionGateDoesNotCacheFailures(t *testing.T) {
	t.Parallel()
	gate := newTorrentSubmissionGate(time.Minute)
	wantErr := errors.New("provider unavailable")
	var calls int

	err := gate.Do(t.Context(), "torbox:hash", func() error {
		calls++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("first submission error = %v, want %v", err, wantErr)
	}
	if doErr := gate.Do(t.Context(), "torbox:hash", func() error {
		calls++
		return nil
	}); doErr != nil {
		t.Fatal(doErr)
	}
	if calls != 2 {
		t.Fatalf("submit calls = %d, want failed submission to remain retryable", calls)
	}
}

// Concurrent callers for one key share a single submission and all receive
// its result, whether it succeeded or failed.
func TestTorrentSubmissionGateCoalescesConcurrentCalls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		outcome error
	}{
		{name: "success", outcome: nil},
		{name: "failure", outcome: errors.New("provider rejected the torrent")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) { checkCoalescedSubmission(t, tc.outcome) })
		})
	}
}

func checkCoalescedSubmission(t *testing.T, outcome error) {
	t.Helper()
	gate := newTorrentSubmissionGate(time.Minute)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64

	submit := func() error {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return outcome
	}

	const workers = 20
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			errs <- gate.Do(t.Context(), "torbox:hash", submit)
		})
	}
	<-started
	// Every caller has joined the in-flight submission before it finishes;
	// a caller arriving after a failure would legitimately submit again.
	synctest.Wait()
	close(release)
	wg.Wait()
	close(errs)

	for err := range errs {
		if !errors.Is(err, outcome) || (outcome == nil && err != nil) {
			t.Fatalf("caller got %v, want the shared result %v", err, outcome)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("submit calls = %d, want 1", got)
	}
}

func TestTorrentSubmissionKeyNormalizesProviderAndHash(t *testing.T) {
	t.Parallel()
	req := NewTorrentRequest(
		" TorBox ",
		"/downloads",
		&utils.Magnet{InfoHash: " ABCDEF "},
		arr.Arr{Name: "sonarr", Host: "", Token: ""},
		"",
		nil,
		"",
		ImportTypeQBit,
		false,
	)
	if got := torrentSubmissionKey(req); got != "torbox:abcdef" {
		t.Fatalf("submission key = %q, want %q", got, "torbox:abcdef")
	}
}
