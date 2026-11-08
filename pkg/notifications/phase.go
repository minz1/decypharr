package notifications

import (
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/state"
)

// phase is where one notifier's actor stands with the event at the head of
// its queue.
type phase uint32

const (
	// phaseIdle: no event is being delivered.
	phaseIdle phase = iota + 1
	// phaseSending: one send of the head event is in flight.
	phaseSending
	// phaseWaiting: the head event's last send failed and a retry timer is armed.
	phaseWaiting
)

func (p phase) String() string {
	switch p {
	case phaseIdle:
		return "idle"
	case phaseSending:
		return "sending"
	case phaseWaiting:
		return "waiting"
	default:
		return "unknown"
	}
}

// phaseSuccessors is the transition table of a notifier. Finishing the head
// with more queued goes Sending → Idle → Sending; a retry timer that cannot be
// armed (the runner is stopping) ends the head, Waiting → Idle.
func phaseSuccessors(p phase) []phase {
	switch p {
	case phaseIdle:
		return []phase{phaseSending}
	case phaseSending:
		return []phase{phaseIdle, phaseWaiting}
	case phaseWaiting:
		return []phase{phaseSending, phaseIdle}
	default:
		return nil
	}
}

// notifierState is the state of one notifier's actor: the event being
// delivered (head), the events behind it, and the current overflow episode.
type notifierState struct {
	phase phase
	head  Event
	// attempt is the number of the head's send in flight or last ended, from 1.
	attempt int
	queue   []Event
	// dropped counts the events the current overflow episode pushed out.
	dropped int
}

// transition is the one way the phase changes. An illegal move is refused,
// logged and reported as false.
func (st *notifierState) transition(to phase, logger zerolog.Logger) bool {
	if !state.Legal(phaseSuccessors, st.phase, to) {
		logger.Error().Stringer("from", st.phase).Stringer("to", to).Msg("Notifier refused an illegal phase change")
		return false
	}
	st.phase = to
	return true
}
