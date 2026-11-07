package rclone

import "strconv"

// rcdState is where the rclone rcd process is in its life. One owner task moves
// it, through rcdOwner.transition, and every other reader only loads it.
//
//	New → Starting → Ready
//	New → Stopped                   (stopped before it was started)
//	Starting, Ready → Restarting    (a failed attempt, an exit while Ready)
//	Starting, Ready → Failed
//	Restarting → Starting           (the backoff ended)
//	Starting, Ready, Restarting, Failed → Stopping → Stopped
//
// Failed (the restart budget is spent, or the binary cannot run) waits for the
// stop.
type rcdState uint32

const (
	// rcdNew has not been started.
	rcdNew rcdState = iota + 1
	// rcdStarting has an rcd process that has not answered a ping yet.
	rcdStarting
	// rcdReady has an rcd process that answered a ping.
	rcdReady
	// rcdRestarting waits out a backoff with no process running.
	rcdRestarting
	// rcdStopping is stopping the process, for good.
	rcdStopping
	// rcdStopped has no process and never starts one again.
	rcdStopped
	// rcdFailed gave up: no process runs and none will start.
	rcdFailed
)

func (s rcdState) String() string {
	switch s {
	case rcdNew:
		return "new"
	case rcdStarting:
		return "starting"
	case rcdReady:
		return "ready"
	case rcdRestarting:
		return "restarting"
	case rcdStopping:
		return "stopping"
	case rcdStopped:
		return "stopped"
	case rcdFailed:
		return "failed"
	default:
		return "rcd(" + strconv.FormatUint(uint64(s), 10) + ")"
	}
}

// rcdSuccessors is the transition table.
func rcdSuccessors(s rcdState) []rcdState {
	switch s {
	case rcdNew:
		return []rcdState{rcdStarting, rcdStopped}
	case rcdStarting:
		return []rcdState{rcdReady, rcdRestarting, rcdFailed, rcdStopping}
	case rcdReady:
		return []rcdState{rcdRestarting, rcdFailed, rcdStopping}
	case rcdRestarting:
		return []rcdState{rcdStarting, rcdStopping}
	case rcdFailed:
		return []rcdState{rcdStopping}
	case rcdStopping:
		return []rcdState{rcdStopped}
	case rcdStopped:
		return nil
	default:
		return nil
	}
}
