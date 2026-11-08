package retry_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/retry"
)

var errBoom = errors.New("boom")

func TestDoExhaustsAttempts(t *testing.T) {
	t.Parallel()
	calls := 0
	err := retry.Do(context.Background(), func() error { calls++; return errBoom }, retry.Attempts(3))
	if !errors.Is(err, errBoom) || calls != 3 {
		t.Fatalf("err=%v calls=%d, want boom after 3", err, calls)
	}
}

func TestDoStopsOnUnrecoverable(t *testing.T) {
	t.Parallel()
	calls := 0
	err := retry.Do(
		context.Background(),
		func() error { calls++; return retry.Unrecoverable(errBoom) },
		retry.Attempts(3),
	)
	// Unrecoverable must hand back boom itself, not a wrapper around it.
	if !errors.Is(err, errBoom) || errors.Unwrap(err) != nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want bare boom after 1", err, calls)
	}
}

func TestDoHonorsIf(t *testing.T) {
	t.Parallel()
	calls := 0
	err := retry.Do(
		context.Background(),
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
	err := retry.Do(context.Background(), func() error {
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

// A ctx that ends while Do waits out its delay stops the retry at once: the
// fn is called once and Do returns the ctx error, not a later attempt.
func TestDoReturnsAtCancel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := 0
		time.AfterFunc(time.Second, cancel)
		start := time.Now()
		err := retry.Do(
			ctx,
			func() error { calls++; return errBoom },
			retry.Attempts(3),
			retry.Delay(time.Hour),
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1", calls)
		}
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Fatalf("Do returned after %s, want 1s", elapsed)
		}
	})
}
