package rclone

import (
	"strconv"
	"time"

	"github.com/sirrobot01/decypharr/internal/state"
)

// mountState is where the rclone mount is in its life. The mount supervisor
// moves it while the manager runs, and the rcd owner's stop moves it last;
// both go through moveMount, and every other reader only loads the view.
//
//	Waiting → Mounting                       (rcd became Ready)
//	Mounting → Mounting                      (an attempt failed, attempts left)
//	Mounting → Mounted | Failed              (an attempt worked; the round is spent)
//	Mounted → Degraded                       (a probe failed)
//	Degraded → Degraded | Mounted            (another failed probe; a good one)
//	Degraded → Mounting                      (the mount point is gone twice in a row)
//	Failed → Failed | Mounting               (the backend is down; it answers again)
//	Mounting, Mounted, Degraded, Failed → Waiting   (rcd died)
//	Waiting, Mounting, Mounted, Degraded, Failed → Unmounting → Unmounted
//	Waiting → Unmounted                      (stopped before any attempt)
type mountState uint32

const (
	// mountWaiting waits for rcd to be Ready.
	mountWaiting mountState = iota + 1
	// mountMounting has a mount attempt running or its retry pending.
	mountMounting
	// mountMounted is mounted and probed.
	mountMounted
	// mountDegraded is mounted, but its last probe failed.
	mountDegraded
	// mountFailed spent a round of attempts and waits to try another.
	mountFailed
	// mountUnmounting is the stop's unmount running.
	mountUnmounting
	// mountUnmounted is the end.
	mountUnmounted
)

func (s mountState) String() string {
	switch s {
	case mountWaiting:
		return "waiting"
	case mountMounting:
		return "mounting"
	case mountMounted:
		return "mounted"
	case mountDegraded:
		return "degraded"
	case mountFailed:
		return "failed"
	case mountUnmounting:
		return "unmounting"
	case mountUnmounted:
		return "unmounted"
	default:
		return "mount(" + strconv.FormatUint(uint64(s), 10) + ")"
	}
}

// mountSuccessors is the transition table.
func mountSuccessors(s mountState) []mountState {
	switch s {
	case mountWaiting:
		return []mountState{mountMounting, mountUnmounting, mountUnmounted}
	case mountMounting:
		return []mountState{mountMounting, mountMounted, mountFailed, mountWaiting, mountUnmounting}
	case mountMounted:
		return []mountState{mountDegraded, mountWaiting, mountUnmounting}
	case mountDegraded:
		return []mountState{mountDegraded, mountMounted, mountMounting, mountWaiting, mountUnmounting}
	case mountFailed:
		return []mountState{mountFailed, mountMounting, mountWaiting, mountUnmounting}
	case mountUnmounting:
		return []mountState{mountUnmounted}
	case mountUnmounted:
		return nil
	default:
		return nil
	}
}

// mountView is the mount's state with what goes with it. A stored view is
// never changed: a move stores a new one, so state, error and time change
// together in one compare-and-swap.
type mountView struct {
	state mountState
	// err is the last failure; only a move into Mounted clears it.
	err       string
	mountedAt time.Time
	// attempted is set by the first move into Mounting: from then on a
	// mount may exist, and the stop must unmount.
	attempted bool
}

// moveMount moves the view to to, applying mutate (which may be nil) to the
// new view. It returns false, changing nothing, when the table does not allow
// the move or a second writer won twice.
func (m *Manager) moveMount(to mountState, mutate func(*mountView)) bool {
	return m.moveMountWith(func(mountView) (mountState, func(*mountView)) { return to, mutate })
}

// moveMountWith is moveMount for a writer whose target depends on the view
// it loaded: pick runs on each try.
func (m *Manager) moveMountWith(pick func(cur mountView) (mountState, func(*mountView))) bool {
	for range 2 {
		cur := m.view.Load()
		to, mutate := pick(*cur)
		if !state.Legal(mountSuccessors, cur.state, to) {
			m.mountRefused.Add(1)
			return false
		}
		next := *cur
		next.state = to
		if mutate != nil {
			mutate(&next)
		}
		if m.view.CompareAndSwap(cur, &next) {
			return true
		}
	}
	m.mountRefused.Add(1)
	return false
}

// mountInfo projects the view for Stats; nil before any mount attempt.
func (m *Manager) mountInfo() *MountInfo {
	v := m.view.Load()
	if v == nil || !v.attempted {
		return nil
	}
	up := v.state == mountMounted || v.state == mountDegraded
	info := &MountInfo{
		LocalPath:  m.mount.MountPath,
		WebDAVURL:  m.webdavURL,
		Mounted:    up,
		ConfigName: ConfigName,
		Error:      v.err,
		State:      v.state.String(),
	}
	if up && !v.mountedAt.IsZero() {
		info.MountedAt = v.mountedAt.Format(time.RFC3339)
	}
	return info
}
