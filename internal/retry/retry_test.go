package retry_test

import (
	"errors"
	"testing"

	"github.com/sirrobot01/decypharr/internal/retry"
)

var errBoom = errors.New("boom")

func TestDoExhaustsAttempts(t *testing.T) {
	t.Parallel()
	calls := 0
	err := retry.Do(func() error { calls++; return errBoom }, retry.Attempts(3))
	if !errors.Is(err, errBoom) || calls != 3 {
		t.Fatalf("err=%v calls=%d, want boom after 3", err, calls)
	}
}

func TestDoStopsOnUnrecoverable(t *testing.T) {
	t.Parallel()
	calls := 0
	err := retry.Do(func() error { calls++; return retry.Unrecoverable(errBoom) }, retry.Attempts(3))
	// Unrecoverable must hand back boom itself, not a wrapper around it.
	if !errors.Is(err, errBoom) || errors.Unwrap(err) != nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want bare boom after 1", err, calls)
	}
}

func TestDoHonorsIf(t *testing.T) {
	t.Parallel()
	calls := 0
	err := retry.Do(
		func() error { calls++; return errBoom },
		retry.Attempts(3),
		retry.If(func(error) bool { return false }),
	)
	if !errors.Is(err, errBoom) || calls != 1 {
		t.Fatalf("err=%v calls=%d, want boom after 1", err, calls)
	}
}

func TestDoRecovers(t *testing.T) {
	t.Parallel()
	calls := 0
	err := retry.Do(func() error {
		calls++
		if calls < 2 {
			return errBoom
		}
		return nil
	}, retry.Attempts(3))
	if err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
