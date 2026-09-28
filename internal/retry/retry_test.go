package retry_test

import (
	"errors"
	"testing"

	"github.com/sirrobot01/decypharr/internal/retry"
)

func TestDo(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")

	calls := 0
	err := retry.Do(func() error { calls++; return boom }, retry.Attempts(3))
	if !errors.Is(err, boom) || calls != 3 {
		t.Fatalf("exhausted: err=%v calls=%d, want boom after 3", err, calls)
	}

	calls = 0
	err = retry.Do(func() error { calls++; return retry.Unrecoverable(boom) }, retry.Attempts(3))
	if err != boom || calls != 1 { //nolint:errorlint // Unrecoverable must return the unwrapped error itself
		t.Fatalf("unrecoverable: err=%v calls=%d, want bare boom after 1", err, calls)
	}

	calls = 0
	err = retry.Do(func() error { calls++; return boom }, retry.Attempts(3), retry.RetryIf(func(error) bool { return false }))
	if !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("retryIf false: err=%v calls=%d, want boom after 1", err, calls)
	}

	calls = 0
	err = retry.Do(func() error {
		calls++
		if calls < 2 {
			return boom
		}
		return nil
	}, retry.Attempts(3))
	if err != nil || calls != 2 {
		t.Fatalf("recovers: err=%v calls=%d", err, calls)
	}
}
