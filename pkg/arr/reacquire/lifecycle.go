package reacquire

// serviceState is where a Service is in its life. It is guarded by the
// service's lifecycleMu; serviceSuccessors lists the moves it may make.
type serviceState uint32

const (
	// serviceConstructed: the repositories are open, Start has not run.
	serviceConstructed serviceState = iota
	// serviceRunning: Start loaded the stores and the worker is running.
	serviceRunning
	// serviceFailed: Start could not load the stores; operations are refused.
	serviceFailed
	// serviceStopped: the group stopped and the repositories are closed.
	serviceStopped
)

// serviceSuccessors is the transition table of a Service.
func serviceSuccessors(s serviceState) []serviceState {
	switch s {
	case serviceConstructed:
		return []serviceState{serviceRunning, serviceFailed, serviceStopped}
	case serviceRunning, serviceFailed:
		return []serviceState{serviceStopped}
	case serviceStopped:
		return nil
	default:
		return nil
	}
}
