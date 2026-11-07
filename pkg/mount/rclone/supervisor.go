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
)

// supKind is what a supervisor message says.
type supKind uint8

const (
	// supRcdReady: rcd answers.
	supRcdReady supKind = iota + 1
	// supRcdDied: rcd exited while Ready.
	supRcdDied
	// supMountDone: a mount attempt ended, err says how.
	supMountDone
	// supProbeDone: a probe ended, probe and err say how.
	supProbeDone
	// supRetryDue: the retry delay of a failed attempt is over.
	supRetryDue
	// supProbeDue: the next probe is due.
	supProbeDue
)

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
}

// mountSupervisor is the one actor that moves the mount's state while the
// manager runs. Slow work runs in children that report back with messages.
type mountSupervisor struct {
	m *Manager
}

// Activate starts with no child, no timer and a fresh round.
func (mountSupervisor) Activate(context.Context, struct{}) supState { return supState{} }

// Handle applies one event to the mount's state.
func (s mountSupervisor) Handle(
	ctx context.Context,
	_ struct{},
	st *supState,
	msg supMsg,
	fx *keyed.Effects[struct{}, supMsg],
) {
	switch msg.kind {
	case supRcdReady:
		s.rcdReady(ctx, st, fx)
	case supRcdDied:
		s.rcdDied(ctx, st, fx)
	case supMountDone:
		if s.current(st, msg) {
			st.hasChild = false
			s.mountDone(ctx, st, fx, msg.err)
		}
	case supProbeDone:
		if s.current(st, msg) {
			st.hasChild = false
			s.probeDone(ctx, st, fx, msg.probe, msg.err)
		}
	case supRetryDue:
		if s.current(st, msg) {
			st.hasTimer = false
			if s.m.view.Load().state == mountMounting {
				s.spawnMount(st, fx, true, false)
			}
		}
	case supProbeDue:
		if s.current(st, msg) {
			st.hasTimer = false
			s.probeDue(st, fx)
		}
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

func (s mountSupervisor) rcdReady(ctx context.Context, st *supState, fx *keyed.Effects[struct{}, supMsg]) {
	cur := s.m.view.Load()
	if cur.state != mountWaiting {
		s.m.logger.Debug().Str("mount", cur.state.String()).Msg("rcd ready, mount not waiting")
		return
	}
	st.attempts = 0
	if s.enter(ctx, st, fx, mountMounting, func(v *mountView) { v.attempted = true }) {
		s.spawnMount(st, fx, cur.attempted, false)
	}
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
) {
	if s.m.view.Load().state != mountMounting {
		return
	}
	if err == nil {
		if s.enter(ctx, st, fx, mountMounted, func(v *mountView) { v.err = ""; v.mountedAt = time.Now() }) {
			st.gone, st.backoff = 0, 0
			s.schedule(st, fx, probeInterval, supProbeDue)
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
			s.schedule(st, fx, probeInterval, supProbeDue)
			return
		}
		if s.enter(ctx, st, fx, mountDegraded, withErr(err)) {
			st.gone = 0
			next := probeInterval
			if kind == probeMountGone {
				st.gone, next = 1, degradedProbeInterval
			}
			s.schedule(st, fx, next, supProbeDue)
		}
	case mountDegraded:
		s.degradedProbe(ctx, st, fx, kind, err)
	case mountFailed:
		if kind == probeOK || kind == probeMountGone {
			st.attempts = 0
			if s.enter(ctx, st, fx, mountMounting, nil) {
				s.spawnMount(st, fx, true, false)
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
		if s.enter(ctx, st, fx, mountMounted, func(v *mountView) { v.err = "" }) {
			st.gone = 0
			s.schedule(st, fx, probeInterval, supProbeDue)
		}
	case probeMountGone:
		st.gone++
		if st.gone >= goneToRecover {
			st.attempts, st.gone = 0, 0
			if s.enter(ctx, st, fx, mountMounting, withErr(err)) {
				s.spawnMount(st, fx, true, true)
			}
			return
		}
		if s.enter(ctx, st, fx, mountDegraded, withErr(err)) {
			s.schedule(st, fx, degradedProbeInterval, supProbeDue)
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
	st.gone = 0
	if s.enter(ctx, st, fx, mountDegraded, withErr(err)) {
		s.schedule(st, fx, probeInterval, supProbeDue)
	}
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
// forget asks rclone to drop its registration of a gone one.
func (s mountSupervisor) spawnMount(st *supState, fx *keyed.Effects[struct{}, supMsg], cleanup, forget bool) {
	epoch := st.epoch
	id, err := fx.Go("mount", func(ctx context.Context) supMsg {
		ctx, cancel := context.WithTimeout(ctx, mountAttemptTimeout)
		defer cancel()
		return supMsg{kind: supMountDone, epoch: epoch, err: s.m.mountOnce(ctx, cleanup, forget)}
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

// sendSup hands an rcd event to the supervisor. Before Start, or once it
// stops, there is no one to tell.
func (m *Manager) sendSup(kind supKind) {
	if m.sup == nil {
		return
	}
	if err := m.sup.Send(supMsg{kind: kind}); err != nil {
		m.logger.Debug().Err(err).Msg("Mount supervisor did not take an rcd event")
	}
}
