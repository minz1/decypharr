package manager

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/arr"
)

func heldGrabManager() *Manager { return &Manager{logger: zerolog.Nop()} }

// A transient failure, such as an unreadable Arr queue, keeps the grab for another try.
func TestHeldGrabSurvivesATransientError(t *testing.T) {
	t.Parallel()
	m := heldGrabManager()
	calls := 0
	m.holdGrab("hash-a", "sonarr/realdebrid", func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("arr queue unreadable")
		}
		return nil
	})
	for range 2 {
		m.retryHeldGrabs(t.Context())
		if m.heldGrabCount() != 1 {
			t.Fatalf("held grabs after %d tries = %d, want it kept", calls, m.heldGrabCount())
		}
	}
	m.retryHeldGrabs(t.Context())
	if calls != 3 || m.heldGrabCount() != 0 {
		t.Fatalf(
			"calls = %d, held = %d, want it sent on the third try and dropped from the list",
			calls,
			m.heldGrabCount(),
		)
	}
}

// A grab that keeps failing is dropped after a bounded number of tries.
func TestHeldGrabIsDroppedAfterBoundedFailures(t *testing.T) {
	t.Parallel()
	m := heldGrabManager()
	calls := 0
	m.holdGrab("hash-a", "sonarr/realdebrid", func(context.Context) error {
		calls++
		return errors.New("always broken")
	})
	for range maxHeldGrabAttempts + 3 {
		m.retryHeldGrabs(t.Context())
	}
	if calls != maxHeldGrabAttempts || m.heldGrabCount() != 0 {
		t.Fatalf("calls = %d, held = %d, want %d tries and then dropped", calls, m.heldGrabCount(), maxHeldGrabAttempts)
	}
}

// A grab held by one bucket does not stop the drain of grabs in other buckets, nor of the same bucket's later ones.
func TestHeldBucketOnlyStopsItsOwnGrabs(t *testing.T) {
	t.Parallel()
	m := heldGrabManager()
	held := func(context.Context) error { return arr.ErrBlocklistHeld }
	var otherSent, sameAsked int
	m.holdGrab("hash-a", "sonarr/realdebrid", held)
	m.holdGrab("hash-b", "sonarr/realdebrid", func(context.Context) error {
		sameAsked++
		return nil
	})
	m.holdGrab("hash-c", "sonarr/torbox", func(context.Context) error {
		otherSent++
		return nil
	})
	m.retryHeldGrabs(t.Context())
	if otherSent != 1 || sameAsked != 0 {
		t.Fatalf(
			"other bucket sent %d, same bucket asked %d, want the other bucket drained and the held one's followers left",
			otherSent,
			sameAsked,
		)
	}
	if m.heldGrabCount() != 2 {
		t.Fatalf("held grabs = %d, want the 2 of the held bucket", m.heldGrabCount())
	}
}
