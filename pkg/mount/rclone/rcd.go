package rclone

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/state"
	"github.com/sirrobot01/decypharr/internal/task"
)

const (
	// rcdReadyTimeout bounds one attempt's wait for rcd to answer a ping.
	rcdReadyTimeout = 30 * time.Second
	// rcdPingTimeout bounds one ping; rcdPingInterval spaces the pings.
	rcdPingTimeout  = 2 * time.Second
	rcdPingInterval = time.Second
	// rcdMaxRestarts is how many restarts follow the failures since rcd last
	// stayed Ready for rcdStableAfter; one more failure ends in Failed.
	rcdMaxRestarts = 5
	// rcdBackoffBase doubles with every consecutive failure up to rcdBackoffMax.
	rcdBackoffBase = time.Second
	rcdBackoffMax  = 30 * time.Second
	// rcdStableAfter is how long rcd must stay Ready for its failures to be forgotten.
	rcdStableAfter = time.Minute
	// rcdUnmountBudget bounds the unmount that runs before rcd is signalled.
	rcdUnmountBudget = 8 * time.Second
)

// rcdProcess is the running rcd, as the owner sees it.
type rcdProcess interface {
	// Wait blocks until the process exits. The reaper is its only caller.
	Wait() error
	Signal(os.Signal) error
	Kill() error
	Pid() int
}

// rcdAttempt is one spawned process and the channel its reaper closes.
type rcdAttempt struct {
	proc rcdProcess
	// exited is closed by the reaper once Wait returned.
	exited chan struct{}
	// waitErr is Wait's result; it is written before exited closes.
	waitErr error
}

func (a *rcdAttempt) hasExited() bool {
	select {
	case <-a.exited:
		return true
	default:
		return false
	}
}

// rcdOwner runs the rcd process: it spawns it, waits until it answers, restarts
// it a bounded number of times and stops it in order. It is the only writer of
// state. Set the fields before run; newRcdOwner fills them with no-ops.
type rcdOwner struct {
	spawn func() (rcdProcess, error)
	ping  func(ctx context.Context) error
	// onReady runs on every move to Ready.
	onReady func()
	// onExit runs when rcd exits while Ready, before the restart or the failure.
	onExit func()
	// beforeStop runs only when a stop starts from Ready: the unmount, before rcd is signalled.
	beforeStop func(ctx context.Context)
	// closeLog runs once, on every path that ends the owner.
	closeLog func() error
	logger   zerolog.Logger

	state    *state.Atomic[rcdState]
	restarts atomic.Uint64
	// group is the rcd group: the per-process reapers run on it.
	group *task.Group
}

func newRcdOwner(group *task.Group, logger zerolog.Logger) *rcdOwner {
	return &rcdOwner{
		spawn:      func() (rcdProcess, error) { return nil, errors.New("rcd owner has no spawn") },
		ping:       func(context.Context) error { return errors.New("rcd owner has no ping") },
		onReady:    func() {},
		onExit:     func() {},
		beforeStop: func(context.Context) {},
		closeLog:   func() error { return nil },
		logger:     logger,
		state:      state.NewAtomic(rcdNew, rcdSuccessors),
		group:      group,
	}
}

// transition is the only writer of state. A refused move is a bug in the owner.
func (o *rcdOwner) transition(from, to rcdState) bool {
	if o.state.Transition(from, to) {
		o.logger.Debug().Str("from", from.String()).Str("to", to.String()).Msg("rcd state")
		return true
	}
	o.logger.Error().
		Str("from", from.String()).
		Str("to", to.String()).
		Str("current", o.state.Load().String()).
		Msg("rcd state move refused")
	return false
}

// run is the owner task. The state must be Starting. It returns nil once the
// state is Stopped, or an error when it ends in Stopping with a process it
// could not reap.
func (o *rcdOwner) run(ctx context.Context) error {
	if current := o.state.Load(); current != rcdStarting {
		return fmt.Errorf("rcd owner cannot run from state %s", current)
	}
	failures := 0
	for {
		if ctx.Err() != nil {
			return o.stopSequence(rcdStarting, nil)
		}
		res := o.attempt(ctx)
		switch res.end {
		case endStop:
			return o.stopSequence(res.from, res.att)
		case endFatal:
			o.transition(res.from, rcdFailed)
			return o.parked(ctx)
		case endFailed:
		}

		if res.from == rcdReady {
			o.onExit()
		}
		if res.stable {
			failures = 0
		}
		failures++
		if res.att != nil {
			// A failed attempt with a live process stops it before the next one.
			if err := o.stopProcess(res.att); err != nil {
				return o.stopSequence(res.from, nil, err)
			}
		}
		if failures > rcdMaxRestarts {
			o.logger.Error().Int("failures", failures).Msg("rcd keeps failing, giving up")
			o.transition(res.from, rcdFailed)
			return o.parked(ctx)
		}
		o.transition(res.from, rcdRestarting)
		o.restarts.Add(1)
		backoff := min(rcdBackoffBase<<(failures-1), rcdBackoffMax)
		o.logger.Warn().Dur("backoff", backoff).Int("failures", failures).Msg("rcd failed, restarting")
		select {
		case <-ctx.Done():
			return o.stopSequence(rcdRestarting, nil)
		case <-time.After(backoff):
			o.transition(rcdRestarting, rcdStarting)
		}
	}
}

// attemptEnd is how an attempt ended.
type attemptEnd int

const (
	// endStop: the context ended.
	endStop attemptEnd = iota + 1
	// endFatal: the binary cannot run, so no restart can help.
	endFatal
	// endFailed: the process never got Ready, or exited.
	endFailed
)

// attemptResult says how an attempt ended, from which state, and with which process.
type attemptResult struct {
	end attemptEnd
	// from is the state the attempt ended in: Starting, or Ready.
	from rcdState
	// att is the process, nil if none was spawned.
	att *rcdAttempt
	// stable is true when the process stayed Ready for rcdStableAfter.
	stable bool
}

// attempt spawns rcd and follows it until it fails, exits or the context ends.
func (o *rcdOwner) attempt(ctx context.Context) attemptResult {
	att, err := o.launch()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrPermission) {
			o.logger.Error().Err(err).Msg("rcd cannot be started")
			return attemptResult{end: endFatal, from: rcdStarting}
		}
		o.logger.Warn().Err(err).Msg("rcd did not start")
		return attemptResult{end: endFailed, from: rcdStarting}
	}
	res := attemptResult{from: rcdStarting, att: att}
	switch o.awaitReady(ctx, att) {
	case readyStopped:
		res.end = endStop
	case readyFailed:
		res.end = endFailed
	case readyOK:
		if !o.transition(rcdStarting, rcdReady) {
			res.end = endFailed
			return res
		}
		res.from = rcdReady
		readyAt := time.Now()
		o.logger.Info().Int("pid", att.proc.Pid()).Msg("rcd is ready")
		o.onReady()
		select {
		case <-att.exited:
			res.end = endFailed
			res.stable = time.Since(readyAt) >= rcdStableAfter
			o.logger.Warn().Err(att.waitErr).Int("pid", att.proc.Pid()).Msg("rcd exited")
		case <-ctx.Done():
			res.end = endStop
		}
	}
	return res
}

// launch spawns rcd and starts its reaper.
func (o *rcdOwner) launch() (*rcdAttempt, error) {
	proc, err := o.spawn()
	if err != nil {
		return nil, err
	}
	att := &rcdAttempt{proc: proc, exited: make(chan struct{})}
	if goErr := o.group.Go("wait", func(context.Context) error {
		att.waitErr = proc.Wait()
		close(att.exited)
		return nil
	}); goErr != nil {
		if killErr := proc.Kill(); killErr != nil {
			o.logger.Error().Err(killErr).Int("pid", proc.Pid()).Msg("rcd has no reaper and could not be killed")
		}
		return nil, fmt.Errorf("reap rcd pid %d: %w", proc.Pid(), goErr)
	}
	return att, nil
}

// readiness is how the wait for a ping ended.
type readiness int

const (
	// readyOK: a ping succeeded while the process lived.
	readyOK readiness = iota + 1
	// readyFailed: the process exited, or rcdReadyTimeout passed.
	readyFailed
	// readyStopped: the context ended.
	readyStopped
)

// awaitReady pings rcd until one ping succeeds, the process exits, the context
// ends or rcdReadyTimeout passes. A ping never outlives the deadline.
func (o *rcdOwner) awaitReady(ctx context.Context, att *rcdAttempt) readiness {
	deadline := time.Now().Add(rcdReadyTimeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			o.logger.Warn().Int("pid", att.proc.Pid()).Dur("timeout", rcdReadyTimeout).Msg("rcd did not answer in time")
			return readyFailed
		}
		pingCtx, cancel := context.WithTimeout(ctx, min(rcdPingTimeout, remaining))
		err := o.ping(pingCtx)
		cancel()
		switch {
		case ctx.Err() != nil:
			return readyStopped
		case att.hasExited():
			return readyFailed
		case err == nil:
			return readyOK
		}
		select {
		case <-ctx.Done():
			return readyStopped
		case <-att.exited:
			return readyFailed
		case <-time.After(min(rcdPingInterval, max(time.Until(deadline), 0))):
		}
	}
}

// parked waits in Failed for the stop.
func (o *rcdOwner) parked(ctx context.Context) error {
	<-ctx.Done()
	return o.stopSequence(rcdFailed, nil)
}

// stopSequence ends the owner from the state from: Stopping, the unmount (only
// from Ready), the process, the log, then Stopped. A process that cannot be
// reaped leaves the state at Stopping and is returned as the error. earlier is
// an error from a stop that already failed.
func (o *rcdOwner) stopSequence(from rcdState, att *rcdAttempt, earlier ...error) error {
	o.transition(from, rcdStopping)
	if from == rcdReady {
		ctx, cancel := task.Detached(o.group.Context(), rcdUnmountBudget)
		o.beforeStop(ctx)
		cancel()
	}
	err := errors.Join(earlier...)
	if att != nil {
		err = errors.Join(err, o.stopProcess(att))
	}
	if closeErr := o.closeLog(); closeErr != nil {
		o.logger.Warn().Err(closeErr).Msg("rcd log did not close cleanly")
	}
	if err == nil {
		o.transition(rcdStopping, rcdStopped)
	}
	return err
}

// stopProcess signals the process, kills it if it outlives rcloneGracefulStop
// and waits up to rcloneReapTimeout for it to be reaped.
func (o *rcdOwner) stopProcess(att *rcdAttempt) error {
	if att.hasExited() {
		return nil
	}
	pid := att.proc.Pid()
	graceful := true
	if err := att.proc.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
		o.logger.Warn().Err(err).Int("pid", pid).Msg("rcd could not be signalled, killing it")
		o.kill(att)
		graceful = false
	}
	if graceful {
		select {
		case <-att.exited:
			return nil
		case <-time.After(rcloneGracefulStop):
			o.kill(att)
		}
	}
	select {
	case <-att.exited:
		return nil
	case <-time.After(rcloneReapTimeout):
		o.logger.Error().Int("pid", pid).Msg("rcd could not be reaped")
		return fmt.Errorf("rcd could not be reaped: pid %d", pid)
	}
}

func (o *rcdOwner) kill(att *rcdAttempt) {
	if err := att.proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		o.logger.Error().Err(err).Int("pid", att.proc.Pid()).Msg("rcd could not be killed")
	}
}
