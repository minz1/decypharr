package rclone

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

const (
	// mountAttempts is how many mount attempts make a round; a spent round is Failed.
	mountAttempts = 4
	// mountAttemptTimeout bounds one mount attempt, cleanup included.
	mountAttemptTimeout = 60 * time.Second
	// probeTimeout bounds one probe.
	probeTimeout = 10 * time.Second
	// probeInterval is the probe period while Mounted, and after a Degraded probe that is not mountGone.
	probeInterval = 30 * time.Second
	// degradedProbeInterval is how soon a mountGone probe is checked again.
	degradedProbeInterval = 5 * time.Second
	// failedBackoffBase and failedBackoffMax space the rounds: the wait doubles per Failed, and Mounted resets it.
	failedBackoffBase = 30 * time.Second
	failedBackoffMax  = 10 * time.Minute
	// failedBackoffFactor is how much each Failed grows the wait.
	failedBackoffFactor = 2
	// goneToRecover is how many mountGone probes in a row make the supervisor remount.
	goneToRecover = 2
	// watchdogInterval is the probe period while the mount table is watched: the table reports a detach at once,
	// so the probe only catches a hung rclone.
	watchdogInterval = 5 * time.Minute
)

// supKind is what a supervisor message says.
type supKind uint8

const (
	// supRcdChanged: the rcd state changed. It carries nothing: the handler reads the rcd snapshot when it runs, so
	// an exit and the next Ready coalesced into one are still both seen.
	supRcdChanged supKind = iota + 1
	// supMountDone: a mount attempt ended, err says how.
	supMountDone
	// supProbeDone: a probe ended, probe and err say how.
	supProbeDone
	// supRetryDue: the retry delay of a failed attempt is over.
	supRetryDue
	// supProbeDue: the next probe is due.
	supProbeDue
	// supWatchDown: the mount table watcher ended, err says why; the table is not trusted again.
	supWatchDown
	// supTableChanged: the mount table changed. It carries nothing: the handler reads the table when it runs, so a
	// late one sees the table as it is.
	supTableChanged
	// supBackendStopped: the WebDAV server the mount reads from stopped.
	supBackendStopped
)

// supClass makes a table change and an rcd change observations, which a full mailbox coalesces; the rest are
// commands.
func supClass(msg supMsg) keyed.Class {
	if msg.kind == supTableChanged || msg.kind == supRcdChanged {
		return keyed.Observation
	}
	return keyed.Command
}

// supCoalesce makes queued changes of one kind one: the handler reads the table or the rcd snapshot, so one read
// sees them all. The two kinds never overwrite each other.
func supCoalesce(msg supMsg) string {
	if msg.kind == supRcdChanged {
		return "rcd"
	}
	return "table"
}

// probeKind is what a probe found.
type probeKind uint8

const (
	// probeOK: the mount is listed and the backend answers.
	probeOK probeKind = iota + 1
	// probeMountGone: the backend answers, but rclone does not list the mount.
	probeMountGone
	// probeBackendDown: listing the backend through rclone failed.
	probeBackendDown
	// probeRCError: rclone's mount list could not be read.
	probeRCError
)

// supMsg is a message to the mount supervisor. epoch stamps child results and
// timers: one from an older epoch is stale and dropped.
type supMsg struct {
	kind  supKind
	epoch uint64
	err   error
	probe probeKind
	// path is where a mount attempt that worked shows in the mount table.
	path string
}

// supState is the supervisor's own bookkeeping; the mount's state is the
// Manager's view.
type supState struct {
	// epoch is bumped on every move; children and timers carry the epoch they were started in.
	epoch    uint64
	child    keyed.ChildID
	hasChild bool
	timer    keyed.TimerID
	hasTimer bool
	// attempts counts the failed attempts of this round.
	attempts int
	// gone counts mountGone probes in a row.
	gone int
	// backoff is the wait before the next round's probe; zero until the first Failed.
	backoff time.Duration
	// watchUp says the mount table is watched and trusted. It starts true and, once false, stays false.
	watchUp bool
	// tablePath and tableID are where our mount shows in the mount table and its mount ID there: the top of the stack
	// when it was mounted. That ID gone later means our mount was detached, whatever happened to the mounts below it.
	tablePath string
	tableID   int
	// backendStopped says the WebDAV server stopped; once set it stays, and an up mount never stays Mounted.
	backendStopped bool
	// rcdGen is the rcd generation the mount was started through; 0 before the first Ready.
	rcdGen uint64
}

// nextProbe is when an up mount is probed next. A watched table reports a detach itself, so a Mounted mount's probe
// is only a watchdog; a Degraded one is probed at the normal cadence, and a mountGone is checked again soon.
func nextProbe(state mountState, gone int, watchUp bool) time.Duration {
	switch {
	case state == mountDegraded && gone >= 1:
		return degradedProbeInterval
	case state == mountDegraded:
		// A Degraded mount is not repaired by the table: only a probe finds it good again.
		return probeInterval
	case state == mountMounted && watchUp:
		return watchdogInterval
	default:
		return probeInterval
	}
}

// mountSupervisor is the one actor that moves the mount's state while the
// manager runs. Slow work runs in children that report back with messages.
type mountSupervisor struct {
	m *Manager
}

// Activate starts with no child, no timer, a fresh round and the table trusted until told otherwise.
func (mountSupervisor) Activate(context.Context, struct{}) supState { return supState{watchUp: true} }

// Handle applies one event to the mount's state.
func (s mountSupervisor) Handle(
	ctx context.Context,
	_ struct{},
	st *supState,
	msg supMsg,
	fx *keyed.Effects[struct{}, supMsg],
) {
	switch msg.kind {
	case supRcdChanged:
		s.rcdChanged(ctx, st, fx)
	case supMountDone:
		if s.current(st, msg) {
			st.hasChild = false
			s.mountDone(ctx, st, fx, msg.err, msg.path)
		}
	case supProbeDone:
		if s.current(st, msg) {
			st.hasChild = false
			s.probeDone(ctx, st, fx, msg.probe, msg.err)
		}
	case supRetryDue:
		if s.current(st, msg) {
			st.hasTimer = false
			if s.m.view.Load().state == mountMounting && s.rcdUp(ctx, st, fx) {
				s.spawnMount(st, fx, true, true)
			}
		}
	case supProbeDue:
		if s.current(st, msg) {
			st.hasTimer = false
			s.probeDue(st, fx)
		}
	case supWatchDown:
		s.watchDown(st, fx, msg.err)
	case supTableChanged:
		s.tableChanged(ctx, st, fx)
	case supBackendStopped:
		s.backendStopped(ctx, st, fx)
	default:
		s.m.logger.Error().Uint8("kind", uint8(msg.kind)).Msg("Mount supervisor got an unknown message")
	}
}

// current reports whether a child result or timer belongs to the current
// epoch; a stale one is dropped.
func (s mountSupervisor) current(st *supState, msg supMsg) bool {
	if msg.epoch != st.epoch {
		s.m.logger.Debug().Uint8("kind", uint8(msg.kind)).Msg("Dropped a stale mount supervisor message")
		return false
	}
	return true
}

// probeDue probes a mount that is up, or a Failed one whose backoff is over.
func (s mountSupervisor) probeDue(st *supState, fx *keyed.Effects[struct{}, supMsg]) {
	switch s.m.view.Load().state {
	case mountMounted, mountDegraded, mountFailed:
		s.spawnProbe(st, fx)
	case mountWaiting, mountMounting, mountUnmounting, mountUnmounted:
		// No probe outside the up and Failed states; the epoch makes this unreachable.
	default:
	}
}

// nextBackoff is the wait before the next round after another Failed.
func nextBackoff(b time.Duration) time.Duration {
	if b == 0 {
		return failedBackoffBase
	}
	return min(b*failedBackoffFactor, failedBackoffMax)
}

// rcdChanged applies the rcd snapshot: a mount made through an rcd that is gone or replaced goes back to Waiting,
// then a Ready rcd starts the waiting mount.
func (s mountSupervisor) rcdChanged(ctx context.Context, st *supState, fx *keyed.Effects[struct{}, supMsg]) {
	snap := s.m.owner.snapshot()
	if snap.state != rcdReady || snap.gen != st.rcdGen {
		s.rcdDied(ctx, st, fx)
	}
	if snap.state == rcdReady {
		s.rcdReady(ctx, st, fx, snap.gen)
	}
}

// rcdCurrent reports whether the rcd the mount was started through is still the Ready one.
func (s mountSupervisor) rcdCurrent(st *supState) bool {
	snap := s.m.owner.snapshot()
	return snap.state == rcdReady && snap.gen == st.rcdGen
}

// rcdUp guards every spawn but the rcd report's own: when the rcd is gone or replaced, it moves the mount to Waiting
// for the rcd report to restart it, and says no.
func (s mountSupervisor) rcdUp(ctx context.Context, st *supState, fx *keyed.Effects[struct{}, supMsg]) bool {
	if s.rcdCurrent(st) {
		return true
	}
	s.rcdDied(ctx, st, fx)
	return false
}

func (s mountSupervisor) rcdReady(
	ctx context.Context,
	st *supState,
	fx *keyed.Effects[struct{}, supMsg],
	gen uint64,
) {
	cur := s.m.view.Load()
	if cur.state != mountWaiting {
		s.m.logger.Debug().Str("mount", cur.state.String()).Msg("rcd ready, mount not waiting")
		return
	}
	st.attempts = 0
	st.rcdGen = gen
	// A fresh rcd holds no mount, so its first attempt has nothing to forget. Before any attempt, the mount point is
	// detached only when the table shows a FUSE mount there (a killed process's dead endpoint): a bind or fstab
	// mount at the path is the user's and must stay.
	if s.enter(ctx, st, fx, mountMounting, func(v *mountView) { v.attempted = true }) {
		s.spawnMount(st, fx, cur.attempted || s.staleFUSE(), false)
	}
}

// staleFUSE reports whether the mount table shows a FUSE mount at the mount point. Without a table it says no.
func (s mountSupervisor) staleFUSE() bool {
	entries, err := s.m.readTable(tablePathFor(s.m.mount.MountPath))
	return err == nil && len(entries) > 0
}

func (s mountSupervisor) rcdDied(ctx context.Context, st *supState, fx *keyed.Effects[struct{}, supMsg]) {
	switch s.m.view.Load().state {
	case mountMounting, mountMounted, mountDegraded, mountFailed:
		s.enter(ctx, st, fx, mountWaiting, func(v *mountView) { v.err = "rclone exited" })
	case mountWaiting, mountUnmounting, mountUnmounted:
		// Nothing is mounted through this rcd.
	default:
	}
}

func (s mountSupervisor) mountDone(
	ctx context.Context,
	st *supState,
	fx *keyed.Effects[struct{}, supMsg],
	err error,
	path string,
) {
	if s.m.view.Load().state != mountMounting {
		return
	}
	if err == nil {
		if s.enter(ctx, st, fx, mountMounted, func(v *mountView) { v.err = ""; v.mountedAt = time.Now() }) {
			st.gone, st.backoff = 0, 0
			s.selfCheck(st, path)
			if st.backendStopped {
				s.degradeForBackend(ctx, st, fx)
				return
			}
			s.schedule(st, fx, nextProbe(mountMounted, 0, st.watchUp), supProbeDue)
		}
		return
	}
	st.attempts++
	if st.attempts < mountAttempts {
		if s.enter(ctx, st, fx, mountMounting, withErr(err)) {
			s.m.logger.Warn().Err(err).Int("attempt", st.attempts).Msg("Mount attempt failed, retrying")
			s.schedule(st, fx, config.DefaultRetryDelay, supRetryDue)
		}
		return
	}
	st.backoff = nextBackoff(st.backoff)
	if s.enter(ctx, st, fx, mountFailed, withErr(err)) {
		s.m.logger.Error().Err(err).Dur("next_round_in", st.backoff).Msg("Mount failed")
		s.schedule(st, fx, st.backoff, supProbeDue)
	}
}

func (s mountSupervisor) probeDone(
	ctx context.Context,
	st *supState,
	fx *keyed.Effects[struct{}, supMsg],
	kind probeKind,
	err error,
) {
	switch s.m.view.Load().state {
	case mountMounted:
		if kind == probeOK {
			s.schedule(st, fx, nextProbe(mountMounted, 0, st.watchUp), supProbeDue)
			return
		}
		if s.enter(ctx, st, fx, mountDegraded, withErr(err)) {
			st.gone = 0
			if kind == probeMountGone {
				st.gone = 1
			}
			s.schedule(st, fx, nextProbe(mountDegraded, st.gone, st.watchUp), supProbeDue)
		}
	case mountDegraded:
		s.degradedProbe(ctx, st, fx, kind, err)
	case mountFailed:
		if st.backendStopped {
			// No round can mount against the stopped server; the stopped server is the error.
			err = errors.New("WebDAV server stopped")
		} else if kind == probeOK || kind == probeMountGone {
			if !s.rcdUp(ctx, st, fx) {
				return
			}
			st.attempts = 0
			if s.enter(ctx, st, fx, mountMounting, nil) {
				s.spawnMount(st, fx, true, true)
			}
			return
		}
		st.backoff = nextBackoff(st.backoff)
		if s.enter(ctx, st, fx, mountFailed, withErr(err)) {
			s.schedule(st, fx, st.backoff, supProbeDue)
		}
	case mountWaiting, mountMounting, mountUnmounting, mountUnmounted:
		// A probe runs only in the up and Failed states; the epoch makes this unreachable.
	default:
	}
}

func (s mountSupervisor) degradedProbe(
	ctx context.Context,
	st *supState,
	fx *keyed.Effects[struct{}, supMsg],
	kind probeKind,
	err error,
) {
	switch kind {
	case probeOK:
		if st.backendStopped {
			s.degradeForBackend(ctx, st, fx)
			return
		}
		if s.enter(ctx, st, fx, mountMounted, func(v *mountView) { v.err = "" }) {
			st.gone = 0
			s.schedule(st, fx, nextProbe(mountMounted, 0, st.watchUp), supProbeDue)
		}
	case probeMountGone:
		if st.backendStopped {
			// A remount would read from the stopped server.
			s.degradeForBackend(ctx, st, fx)
			return
		}
		st.gone++
		if st.gone >= goneToRecover {
			if !s.rcdUp(ctx, st, fx) {
				return
			}
			st.attempts, st.gone = 0, 0
			if s.enter(ctx, st, fx, mountMounting, withErr(err)) {
				s.spawnMount(st, fx, true, true)
			}
			return
		}
		if s.enter(ctx, st, fx, mountDegraded, withErr(err)) {
			s.schedule(st, fx, nextProbe(mountDegraded, st.gone, st.watchUp), supProbeDue)
		}
	case probeBackendDown, probeRCError:
		s.degradedStays(ctx, st, fx, err)
	default:
		// An unknown result is a failed probe.
		s.degradedStays(ctx, st, fx, err)
	}
}

// degradedStays keeps a Degraded mount whose probe failed for a reason a remount cannot fix.
func (s mountSupervisor) degradedStays(
	ctx context.Context,
	st *supState,
	fx *keyed.Effects[struct{}, supMsg],
	err error,
) {
	if st.backendStopped {
		// The stopped server is the cause; a probe failing because of it does not replace it.
		s.degradeForBackend(ctx, st, fx)
		return
	}
	st.gone = 0
	if s.enter(ctx, st, fx, mountDegraded, withErr(err)) {
		s.schedule(st, fx, nextProbe(mountDegraded, 0, st.watchUp), supProbeDue)
	}
}

// watchDown stops trusting the mount table and moves a pending probe of an up mount to the fast cadence. It is a
// cadence change, not a move: the epoch stays.
func (s mountSupervisor) watchDown(st *supState, fx *keyed.Effects[struct{}, supMsg], reason error) {
	if !st.watchUp {
		return
	}
	s.fallBack(st, reason)
	state := s.m.view.Load().state
	if !st.hasTimer || (state != mountMounted && state != mountDegraded) {
		return
	}
	fx.CancelTimer(st.timer)
	next := nextProbe(state, st.gone, st.watchUp)
	s.schedule(st, fx, next, supProbeDue)
	s.m.logger.Debug().Dur("next_probe_in", next).Msg("Mount probe moved to the fast cadence")
}

// backendStopped marks the WebDAV server stopped. An up mount goes Degraded and stays there; a remount cannot bring
// the server back. In other states only the flag is kept, for mountDone to apply.
func (s mountSupervisor) backendStopped(ctx context.Context, st *supState, fx *keyed.Effects[struct{}, supMsg]) {
	st.backendStopped = true
	switch s.m.view.Load().state {
	case mountMounted, mountDegraded:
		s.degradeForBackend(ctx, st, fx)
	case mountWaiting, mountMounting, mountFailed, mountUnmounting, mountUnmounted:
	default:
	}
}

// degradeForBackend moves an up mount to Degraded for the stopped WebDAV server and schedules its next probe.
func (s mountSupervisor) degradeForBackend(ctx context.Context, st *supState, fx *keyed.Effects[struct{}, supMsg]) {
	st.gone = 0
	if s.enter(ctx, st, fx, mountDegraded, withErr(errors.New("WebDAV server stopped"))) {
		s.schedule(st, fx, nextProbe(mountDegraded, 0, st.watchUp), supProbeDue)
	}
}

// selfCheck reads the table right after a mount worked: a table that does not show the mount at path (a symlinked
// mount point, a detach before this) cannot report its detach, so the supervisor stops trusting it.
func (s mountSupervisor) selfCheck(st *supState, path string) {
	if !st.watchUp {
		return
	}
	entries, err := s.m.readTable(path)
	id, ok := topEntry(entries)
	if err == nil && !ok {
		err = fmt.Errorf("the mount table shows no FUSE mount at %s", path)
	}
	if err != nil {
		s.fallBack(st, err)
		return
	}
	st.tablePath, st.tableID = path, id
}

// tableChanged reads the mount table now, not when the change was seen: a change queued behind a repair finds the
// mount back and does nothing. Our mount ID gone from the table is a detach: remount at once.
func (s mountSupervisor) tableChanged(ctx context.Context, st *supState, fx *keyed.Effects[struct{}, supMsg]) {
	state := s.m.view.Load().state
	if !st.watchUp || (state != mountMounted && state != mountDegraded) {
		return
	}
	entries, err := s.m.readTable(st.tablePath)
	if err != nil {
		s.watchDown(st, fx, err)
		return
	}
	if hasEntry(entries, st.tableID) {
		s.m.logger.Debug().Int("mount_id", st.tableID).Msg("Mount table changed; our mount is still there")
		return
	}
	if st.backendStopped {
		s.degradeForBackend(ctx, st, fx)
		return
	}
	if !s.rcdUp(ctx, st, fx) {
		return
	}
	st.attempts, st.gone = 0, 0
	if s.enter(ctx, st, fx, mountMounting, withErr(errors.New("mount point detached"))) {
		s.spawnMount(st, fx, true, true)
	}
}

// fallBack stops trusting the mount table for the rest of the supervisor's life; the probe cadence is today's.
func (s mountSupervisor) fallBack(st *supState, reason error) {
	if !st.watchUp {
		return
	}
	st.watchUp = false
	ev := s.m.logger.Warn()
	if errors.Is(reason, errNoMountTable) {
		ev = s.m.logger.Debug()
	}
	ev.Err(reason).Dur("probe_every", probeInterval).Msg("Mount table events are off; probing the mount instead")
}

// withErr records err as the mount's last failure.
func withErr(err error) func(*mountView) {
	return func(v *mountView) {
		if err != nil {
			v.err = err.Error()
		}
	}
}

// enter moves the mount to to. It first drops the running child and the
// pending timer, whose results belong to the state being left. It returns
// false when the move is refused; the caller then starts nothing.
func (s mountSupervisor) enter(
	ctx context.Context,
	st *supState,
	fx *keyed.Effects[struct{}, supMsg],
	to mountState,
	mutate func(*mountView),
) bool {
	if st.hasChild {
		fx.CancelChild(st.child)
		st.hasChild = false
	}
	if st.hasTimer {
		fx.CancelTimer(st.timer)
		st.hasTimer = false
	}
	st.epoch++
	from := s.m.view.Load().state
	if !s.m.moveMount(to, mutate) {
		ev := s.m.logger.Error()
		if ctx.Err() != nil {
			ev = s.m.logger.Warn()
		}
		ev.Str("from", from.String()).Str("to", to.String()).Msg("The mount's state refused a move")
		return false
	}
	if from != to {
		v := s.m.view.Load()
		s.m.logger.Info().Str("from", from.String()).Str("to", to.String()).Str("error", v.err).Msg("Mount state")
	}
	return true
}

// spawnMount starts one mount attempt. cleanup detaches a stale mount first;
// forget asks rclone to drop its registration of a gone one, or of an earlier
// attempt that rclone finished after our timeout. Every attempt but a fresh
// rcd's first forgets.
func (s mountSupervisor) spawnMount(st *supState, fx *keyed.Effects[struct{}, supMsg], cleanup, forget bool) {
	epoch := st.epoch
	id, err := fx.Go("mount", func(ctx context.Context) supMsg {
		ctx, cancel := context.WithTimeout(ctx, mountAttemptTimeout)
		defer cancel()
		if err := s.m.mountOnce(ctx, cleanup, forget); err != nil {
			return supMsg{kind: supMountDone, epoch: epoch, err: err}
		}
		return supMsg{kind: supMountDone, epoch: epoch, path: tablePathFor(s.m.mount.MountPath)}
	}, func(v any) supMsg {
		return supMsg{kind: supMountDone, epoch: epoch, err: fmt.Errorf("mount attempt panicked: %v", v)}
	})
	if err != nil {
		s.m.logger.Debug().Err(err).Msg("Mount attempt not started")
		return
	}
	st.child, st.hasChild = id, true
}

// spawnProbe starts one probe.
func (s mountSupervisor) spawnProbe(st *supState, fx *keyed.Effects[struct{}, supMsg]) {
	epoch := st.epoch
	id, err := fx.Go("probe", func(ctx context.Context) supMsg {
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		kind, probeErr := s.m.probe(ctx)
		return supMsg{kind: supProbeDone, epoch: epoch, probe: kind, err: probeErr}
	}, func(v any) supMsg {
		return supMsg{kind: supProbeDone, epoch: epoch, probe: probeRCError, err: fmt.Errorf("probe panicked: %v", v)}
	})
	if err != nil {
		s.m.logger.Debug().Err(err).Msg("Probe not started")
		return
	}
	st.child, st.hasChild = id, true
}

// schedule delivers a kind message after d, stamped with the current epoch.
func (s mountSupervisor) schedule(st *supState, fx *keyed.Effects[struct{}, supMsg], d time.Duration, kind supKind) {
	st.timer = fx.After(d, supMsg{kind: kind, epoch: st.epoch})
	st.hasTimer = true
}

// probe checks the mount and the backend through rclone. It never stats the
// mount point: a hung FUSE stat cannot be cancelled.
func (m *Manager) probe(ctx context.Context) (probeKind, error) {
	points, listErr := m.client.ListMounts(ctx)
	if err := m.client.CheckMountHealth(ctx, FSName); err != nil {
		return probeBackendDown, err
	}
	if listErr != nil {
		return probeRCError, listErr
	}
	want := filepath.Clean(m.mount.MountPath)
	for _, p := range points {
		if filepath.Clean(p) == want {
			return probeOK, nil
		}
	}
	return probeMountGone, errors.New("mount point not listed by rclone")
}

// sendSup hands an rcd event to the supervisor.
func (m *Manager) sendSup(kind supKind) { m.sendSupMsg(supMsg{kind: kind}) }

// sendSupMsg hands an event to the supervisor. Before Start, or once it stops, there is no one to tell.
func (m *Manager) sendSupMsg(msg supMsg) {
	if m.sup == nil {
		return
	}
	if err := m.sup.Send(msg); err != nil {
		ev := m.logger.Debug()
		if errors.Is(err, keyed.ErrMailboxFull) {
			ev = m.logger.Warn()
		}
		ev.Err(err).Uint8("kind", uint8(msg.kind)).Msg("Mount supervisor did not take an event")
	}
}

// runTableWatch is the "mountinfo" task: it watches the mount table until ctx ends. If the watch ends first, the
// supervisor stops trusting the table; the task itself has not failed.
func (m *Manager) runTableWatch(ctx context.Context) error {
	m.tableWatchEnded(ctx, m.watchTable(ctx, m.tableChanged))
	return nil
}

// tableWatchEnded tells the supervisor the watch ended, unless it ended because ctx did.
func (m *Manager) tableWatchEnded(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	if err == nil {
		err = errors.New("mount table watch ended")
	}
	m.sendSupMsg(supMsg{kind: supWatchDown, err: err})
}

// runBackendWatch is the "backend" task: it tells the supervisor once the WebDAV server stops.
func (m *Manager) runBackendWatch(ctx context.Context) error {
	select {
	case <-m.backendStopped:
		m.sendSup(supBackendStopped)
	case <-ctx.Done():
	}
	return nil
}

// tableChanged is the watcher's callback. It sends no payload: the supervisor reads the table itself.
func (m *Manager) tableChanged() { m.sendSupMsg(supMsg{kind: supTableChanged}) }
