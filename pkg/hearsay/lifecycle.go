package hearsay

// lifecycle is where a Service is in its life. It is held in a state.Atomic so
// Start's starting to ready move under the operation guard's read lock is not a
// data race; lifecycleSuccessors lists the moves it may make.
type lifecycle uint32

const (
	// constructed: the engine is open and Start has not run (or participation is off).
	constructed lifecycle = iota
	// starting: Start is opening the transport node.
	starting
	// ready: the node is stored and the syncer task is running.
	ready
	// failed: Start could not open the transport; the engine still answers.
	failed
	// stopping: the group is stopping; engine calls return their inert value.
	stopping
	// stopped: the close task closed the node and the engine.
	stopped
)

// lifecycleSuccessors is the transition table of a Service.
func lifecycleSuccessors(s lifecycle) []lifecycle {
	switch s {
	case constructed:
		return []lifecycle{starting, stopping}
	case starting:
		return []lifecycle{ready, failed, stopping}
	case ready, failed:
		return []lifecycle{stopping}
	case stopping:
		return []lifecycle{stopped}
	case stopped:
		return nil
	default:
		return nil
	}
}

// String names the state as Status reports it.
func (s lifecycle) String() string {
	switch s {
	case constructed:
		return "constructed"
	case starting:
		return "starting"
	case ready:
		return "ready"
	case failed:
		return "failed"
	case stopping:
		return "stopping"
	case stopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// engineOpen reports whether the engine can still be called in this state.
func (s lifecycle) engineOpen() bool {
	return s < stopping
}
