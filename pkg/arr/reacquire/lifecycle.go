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

// indexerState is where an Indexer is in its life. It is guarded by the indexer's lifecycleMu;
// indexerSuccessors lists the moves it may make.
type indexerState uint32

const (
	// indexerConstructed: Start has not run.
	indexerConstructed indexerState = iota
	// indexerRunning: the loop task is running and requests are queued.
	indexerRunning
	// indexerStopped: the loop ended or its group refused it; requests are refused.
	indexerStopped
)

// indexerSuccessors is the transition table of an Indexer. Start loads nothing that can fail, so unlike a
// Service it has no failed state.
func indexerSuccessors(s indexerState) []indexerState {
	switch s {
	case indexerConstructed:
		return []indexerState{indexerRunning, indexerStopped}
	case indexerRunning:
		return []indexerState{indexerStopped}
	case indexerStopped:
		return nil
	default:
		return nil
	}
}

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
