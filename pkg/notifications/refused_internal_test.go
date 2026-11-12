package notifications

import (
	"testing"

	"github.com/rs/zerolog"
)

// A notifier's illegal phase move is counted on its service.
func TestIllegalPhaseMoveCountsOnTheService(t *testing.T) {
	t.Parallel()
	s := &Service{}
	st := notifierState{phase: phaseIdle, refused: &s.refused}
	if st.transition(phaseWaiting, zerolog.Nop()) {
		t.Fatal("Idle → Waiting was allowed")
	}
	if got := s.Refused(); got != 1 {
		t.Fatalf("Refused() = %d, want 1", got)
	}
}
