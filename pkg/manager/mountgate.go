package manager

import (
	"errors"
	"strconv"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/state"
)

// ErrMountStopping ends a mount-dependent step because the generation's mount is stopping. It is resumable: the
// entry stays queued for the next generation. It is standalone on purpose: it does not wrap task.ErrStopped,
// whose readers mean "my own group is stopping".
var ErrMountStopping = errors.New("mount is stopping")

// stoppingName names the Stopping state of both the manager lifecycle and the mount gate.
const stoppingName = "stopping"

// mountGateState is the mount gate's state.
//
//	Serving → Stopping
//
// Stopping is terminal: a generation never remounts after its stop began.
type mountGateState uint32

const (
	// mountGateServing is the mount serving, or never started.
	mountGateServing mountGateState = iota + 1
	// mountGateStopping is the mount's stop under way or done.
	mountGateStopping
)

func (s mountGateState) String() string {
	switch s {
	case mountGateServing:
		return "serving"
	case mountGateStopping:
		return stoppingName
	default:
		return "mountGateState(" + strconv.FormatUint(uint64(s), 10) + ")"
	}
}

// mountGateSuccessors is the transition table.
func mountGateSuccessors(s mountGateState) []mountGateState {
	switch s {
	case mountGateServing:
		return []mountGateState{mountGateStopping}
	case mountGateStopping:
		return nil
	default:
		return nil
	}
}

// MountGate tells the manager's mount-dependent steps that the mount is stopping. The generation's owner makes one
// per generation and closes it before the mount's own stop begins. A nil gate never closes.
type MountGate struct {
	state  *state.Atomic[mountGateState]
	closed chan struct{}
	logger zerolog.Logger
}

// NewMountGate returns a gate in the Serving state.
func NewMountGate(log zerolog.Logger) *MountGate {
	return &MountGate{
		state:  state.NewAtomic(mountGateServing, mountGateSuccessors),
		closed: make(chan struct{}),
		logger: log,
	}
}

// Stopping is closed once the gate moved to Stopping. A nil gate returns a channel that never closes.
func (g *MountGate) Stopping() <-chan struct{} {
	if g == nil {
		return nil
	}
	return g.closed
}

// IsStopping reports whether the gate moved to Stopping.
func (g *MountGate) IsStopping() bool {
	return g != nil && g.state.Load() == mountGateStopping
}

// transition moves the gate from one state to another and refuses, with a log line, any move the table does not
// list. It reports whether this call made the move.
func (g *MountGate) transition(from, to mountGateState) bool {
	if !state.Legal(mountGateSuccessors, from, to) {
		g.logger.Error().Str("from", from.String()).Str("to", to.String()).Msg("Refused an illegal mount gate move")
		g.state.Transition(from, to) // counts the refusal
		return false
	}
	return g.state.Transition(from, to)
}

// markStopping closes the gate. A gate already Stopping returns silently.
func (g *MountGate) markStopping() {
	if g == nil || g.state.Load() == mountGateStopping {
		return
	}
	if g.transition(mountGateServing, mountGateStopping) {
		close(g.closed)
	}
}

// BeginStop closes the gate: the generation's owner calls it before the mount's own stop begins. A gate already
// Stopping returns silently.
func (g *MountGate) BeginStop() { g.markStopping() }

// SetMountGate installs the generation's gate. Call it before Start; nil keeps the default.
func (m *Manager) SetMountGate(g *MountGate) {
	if g != nil {
		m.mountGate = g
	}
}
