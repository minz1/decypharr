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
