package manager

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sirrobot01/decypharr/internal/customerror"
)

// Every wait may follow every other; observe reports the one it replaced, so a caller logs exactly the changes, and
// waitNone forgets the entry.
func TestProviderWaitsReportEachChange(t *testing.T) {
	t.Parallel()
	all := []providerWait{waitNone, waitConverting, waitPending}
	for _, from := range all {
		for _, to := range all {
			var waits providerWaits
			waits.observe("e", from)
			if got := waits.observe("e", to); got != from {
				t.Errorf("%s -> %s reported %s replaced, want %s", from, to, got, from)
			}
			if got := waits.observe("e", to); got != to {
				t.Errorf("%s again reported %s replaced, want %s (no change)", to, got, to)
			}
			if _, kept := waits.byID["e"]; kept != (to != waitNone) {
				t.Errorf("after %s the entry is kept = %v, want %v", to, kept, to != waitNone)
			}
		}
	}
}

func TestProviderWaitOfClassifiesStatusErrors(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]providerWait{
		nil:                                     waitNone,
		errors.New("realdebrid API error: 502"): waitNone,
		customerror.ErrProviderStatusTimeout:    waitPending,
		fmt.Errorf("x: %w: %w", customerror.ErrMagnetNotConverted, customerror.ErrProviderStatusTimeout): waitConverting,
	} {
		if got := providerWaitOf(err); got != want {
			t.Errorf("providerWaitOf(%v) = %s, want %s", err, got, want)
		}
	}
}
