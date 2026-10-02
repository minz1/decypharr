package customerror_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sirrobot01/decypharr/internal/customerror"
)

func TestPanicErrorPreservesPayload(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("failed")
	for _, payload := range []any{"text", 42, nil, sentinel} {
		err := customerror.NewPanicError(payload)
		if got, want := err.Error(), fmt.Sprintf("panic: %v", payload); got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
		cause, _ := payload.(error)
		if got := errors.Unwrap(err); !errors.Is(got, cause) {
			t.Errorf("Unwrap() = %v, want %v", got, cause)
		}
		if !customerror.IsPanicError(fmt.Errorf("worker: %w", err)) {
			t.Error("wrapped panic was not recognized")
		}
	}
	if !errors.Is(customerror.NewPanicError(sentinel), sentinel) {
		t.Error("error identity was lost")
	}
}
