package external

import "strconv"

// extState is where the external manager is in confirming that the user's
// rclone RC server answers. The probe task moves it, through Manager.transition.
//
//	New → Starting → Ready
//	Starting → Failed             (no ping answered)
//	New, Starting, Ready, Failed → Stopped
type extState uint32

const (
	// extNew has not been started.
	extNew extState = iota + 1
	// extStarting is pinging the RC server.
	extStarting
	// extReady has had a ping answered.
	extReady
	// extFailed pinged the RC server and got no answer; it does not retry.
	extFailed
	// extStopped is finished.
	extStopped
)

func (s extState) String() string {
	switch s {
	case extNew:
		return "new"
	case extStarting:
		return "starting"
	case extReady:
		return "ready"
	case extFailed:
		return "failed"
	case extStopped:
		return "stopped"
	default:
		return "ext(" + strconv.FormatUint(uint64(s), 10) + ")"
	}
}

// extSuccessors is the transition table.
func extSuccessors(s extState) []extState {
	switch s {
	case extNew:
		return []extState{extStarting, extStopped}
	case extStarting:
		return []extState{extReady, extFailed, extStopped}
	case extReady:
		return []extState{extStopped}
	case extFailed:
		return []extState{extStopped}
	case extStopped:
		return nil
	default:
		return nil
	}
}
