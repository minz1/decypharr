package customerror_test

import (
	"errors"
	"io"
	"testing"

	"github.com/sirrobot01/decypharr/internal/customerror"
)

func TestIsSilentError(t *testing.T) {
	t.Parallel()
	// nil used to reach err.Error() and panic; IsRetriableError and
	// IsPermanentError already treated nil as "no".
	if customerror.IsSilentError(nil) {
		t.Error("nil reported as silent")
	}
	if !customerror.IsSilentError(io.EOF) {
		t.Error("io.EOF not silent")
	}
	if customerror.IsSilentError(errors.New("boom")) {
		t.Error("plain error reported as silent")
	}
}

// A provider's 401, 403, 404 or 410 will not change on a retry; anything else may.
func TestNewProviderStatusError(t *testing.T) {
	t.Parallel()
	for status, permanent := range map[int]bool{401: true, 403: true, 404: true, 410: true, 400: false, 429: false, 500: false, 503: false} {
		err := customerror.NewProviderStatusError(errors.New("realdebrid API error: Status: x"), status)
		if err.IsPermanent() != permanent {
			t.Errorf("status %d: permanent = %v, want %v", status, err.IsPermanent(), permanent)
		}
		if err.Error() != "realdebrid API error: Status: x" {
			t.Errorf("status %d: message = %q, want it unchanged", status, err.Error())
		}
		if transient := status == 429 || status >= 500; customerror.IsRetriableError(err) != transient {
			t.Errorf("status %d: retriable = %v, want %v", status, customerror.IsRetriableError(err), transient)
		}
		if err.StatusCode() != 502 {
			t.Errorf("status %d: decypharr would answer %d, want 502", status, err.StatusCode())
		}
	}
}
