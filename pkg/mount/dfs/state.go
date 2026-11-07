package dfs

import "strconv"

// dfsState is where the DFS mount is in its life. Moves go through the manager's
// one move function; every other reader only loads it.
//
//	New → Starting → Ready
//	New → Stopped                   (stopped before it was started)
//	Starting, Ready → Failed        (a start failure, the serve loop died)
//	Starting, Ready → Stopping → Stopped
//	Failed → Stopping → Stopped
//
// Failed waits for the stop.
type dfsState uint32

const (
	// dfsNew has not been started.
	dfsNew dfsState = iota + 1
	// dfsStarting is mounting and has not reported ready yet.
	dfsStarting
	// dfsReady is mounted and serving.
	dfsReady
	// dfsFailed gave up: the mount failed to start, or died while serving.
	dfsFailed
	// dfsStopping is unmounting and closing the cache, for good.
	dfsStopping
	// dfsStopped is unmounted, and never mounts again.
	dfsStopped
)

func (s dfsState) String() string {
	switch s {
	case dfsNew:
		return "new"
	case dfsStarting:
		return "starting"
	case dfsReady:
		return "ready"
	case dfsFailed:
		return "failed"
	case dfsStopping:
		return "stopping"
	case dfsStopped:
		return "stopped"
	default:
		return "dfs(" + strconv.FormatUint(uint64(s), 10) + ")"
	}
}

// dfsSuccessors is the transition table.
func dfsSuccessors(s dfsState) []dfsState {
	switch s {
	case dfsNew:
		return []dfsState{dfsStarting, dfsStopped}
	case dfsStarting:
		return []dfsState{dfsReady, dfsFailed, dfsStopping}
	case dfsReady:
		return []dfsState{dfsFailed, dfsStopping}
	case dfsFailed:
		return []dfsState{dfsStopping}
	case dfsStopping:
		return []dfsState{dfsStopped}
	case dfsStopped:
		return nil
	default:
		return nil
	}
}
