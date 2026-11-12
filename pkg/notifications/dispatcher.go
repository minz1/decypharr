package notifications

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

const (
	// mailboxSize bounds the messages waiting for one notifier's actor.
	mailboxSize = 128
	// queueLimit bounds the events queued behind the one being delivered;
	// past it the oldest queued event is dropped.
	queueLimit = 100
	// neverIdle keeps an actor and its state for the runner's life: an idle
	// exit would drop the state with a retry timer armed.
	neverIdle = -1
)

type msgKind uint8

const (
	// msgEnqueue: Notify handed the actor an event.
	msgEnqueue msgKind = iota + 1
	// msgSendDone: the head's send in flight ended, with err.
	msgSendDone
	// msgRetryDue: the head's retry timer fired.
	msgRetryDue
)

// msg is what one notifier's actor receives.
type msg struct {
	kind msgKind
	ev   Event
	err  error
}

// dropReason says why an event was not delivered.
type dropReason uint8

const (
	// dropFailed: every send allowed failed with a retryable error.
	dropFailed dropReason = iota + 1
	// dropRejected: a send failed in a way sending again cannot fix.
	dropRejected
	// dropOverflow: the queue was full and the event was the oldest in it.
	dropOverflow
	// dropStopped: the runner was stopping and refused the next send or timer.
	dropStopped
)

func (r dropReason) String() string {
	switch r {
	case dropFailed:
		return "failed"
	case dropRejected:
		return "rejected"
	case dropOverflow:
		return "overflow"
	case dropStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// observer hears how each event ended. Tests record it; the service ignores it.
type observer interface {
	delivered(notifier string, ev Event)
	dropped(notifier string, ev Event, reason dropReason, attempts int)
}

type nopObserver struct{}

func (nopObserver) delivered(string, Event)                {}
func (nopObserver) dropped(string, Event, dropReason, int) {}

// dispatcher is the handler of one actor per notifier, keyed by the
// notifier's name: it sends one event at a time, in order, retrying it as
// decide says, with at most queueLimit events behind it.
type dispatcher struct {
	notifiers map[string]Notifier
	logger    zerolog.Logger
	obs       observer
	refused   *atomic.Uint64
}

func newRunner(
	g *task.Group,
	notifiers []Notifier,
	logger zerolog.Logger,
	obs observer,
	refused *atomic.Uint64,
) *keyed.Runner[string, msg, notifierState] {
	d := &dispatcher{
		notifiers: make(map[string]Notifier, len(notifiers)), logger: logger, obs: obs, refused: refused,
	}
	for _, n := range notifiers {
		d.notifiers[n.Name()] = n
	}
	return keyed.New(g, "dispatch", d, keyed.Options[msg]{
		MailboxSize: mailboxSize,
		IdleAfter:   neverIdle,
		Logger:      logger,
	})
}

func (d *dispatcher) Activate(context.Context, string) notifierState {
	return notifierState{phase: phaseIdle, refused: d.refused}
}

func (d *dispatcher) Handle(_ context.Context, key string, st *notifierState, m msg, fx *keyed.Effects[string, msg]) {
	logger := d.logger.With().Str("notifier", key).Logger()
	switch m.kind {
	case msgEnqueue:
		d.enqueue(key, st, m.ev, fx, logger)
	case msgSendDone:
		d.sendDone(key, st, m.err, fx, logger)
	case msgRetryDue:
		if st.phase != phaseWaiting {
			logger.Error().Stringer("phase", st.phase).Msg("Notifier got a retry while not waiting")
			return
		}
		d.startHead(key, st, fx, logger)
	default:
		logger.Error().Uint8("kind", uint8(m.kind)).Msg("Notifier got an unknown message")
	}
}

// enqueue makes ev the head when nothing is being delivered, else queues it
// behind, pushing out the oldest queued event when the queue is full.
func (d *dispatcher) enqueue(
	key string,
	st *notifierState,
	ev Event,
	fx *keyed.Effects[string, msg],
	logger zerolog.Logger,
) {
	if _, ok := d.notifiers[key]; !ok {
		logger.Error().Msg("Notification for an unknown notifier")
		return
	}
	if st.phase == phaseIdle {
		st.head, st.attempt = ev, 0
		d.startHead(key, st, fx, logger)
		return
	}
	st.queue = append(st.queue, ev)
	if len(st.queue) <= queueLimit {
		return
	}
	oldest := st.queue[0]
	st.queue[0] = Event{}
	st.queue = st.queue[1:]
	if st.dropped == 0 {
		logger.Warn().Int("limit", queueLimit).Msg("Notification queue full, dropping the oldest events")
	}
	st.dropped++
	logger.Debug().Str("event", string(oldest.Type)).Msg("Notification dropped from a full queue")
	d.obs.dropped(key, oldest, dropOverflow, 0)
}

// startHead starts the head's next send. When the runner refuses it (it is
// stopping), the head ends as stopped and the next event is tried, until a
// send is in flight or nothing is left.
func (d *dispatcher) startHead(key string, st *notifierState, fx *keyed.Effects[string, msg], logger zerolog.Logger) {
	for {
		if d.spawn(key, st.head, fx) {
			st.attempt++
			st.transition(phaseSending, logger)
			return
		}
		d.drop(key, st, dropStopped, nil, logger)
		if st.phase == phaseWaiting {
			st.transition(phaseIdle, logger)
		}
		if !st.pop(logger) {
			return
		}
	}
}

// spawn runs one send of ev as a child of the actor, bounded by sendTimeout,
// and reports whether the runner took it. A panicking notifier ends the
// event as rejected.
func (d *dispatcher) spawn(key string, ev Event, fx *keyed.Effects[string, msg]) bool {
	n := d.notifiers[key]
	_, err := fx.Go("send",
		func(ctx context.Context) msg {
			ctx, cancel := context.WithTimeout(ctx, sendTimeout)
			defer cancel()
			return msg{kind: msgSendDone, err: n.Send(ctx, ev)}
		},
		func(v any) msg {
			return msg{kind: msgSendDone, err: permanent(fmt.Errorf("notifier panicked: %v", v))}
		})
	return err == nil
}

// sendDone settles the head's send: delivered, retried after decide's wait,
// or dropped; anything but a retry moves on to the next event.
func (d *dispatcher) sendDone(
	key string,
	st *notifierState,
	err error,
	fx *keyed.Effects[string, msg],
	logger zerolog.Logger,
) {
	if st.phase != phaseSending {
		logger.Error().Stringer("phase", st.phase).Msg("Notifier got a send result while not sending")
		return
	}
	retry, wait := decide(st.attempt, err)
	switch {
	case err == nil:
		logger.Trace().Str("event", string(st.head.Type)).Int("attempts", st.attempt).Msg("Notification sent")
		d.obs.delivered(key, st.head)
	case retry && fx.After(wait, msg{kind: msgRetryDue}) != 0:
		st.transition(phaseWaiting, logger)
		logger.Debug().Err(err).Int("attempt", st.attempt).Dur("wait", wait).Msg("Notification send failed, retrying")
		return
	case retry, errors.Is(err, context.Canceled):
		d.drop(key, st, dropStopped, err, logger)
	case retryable(err):
		d.drop(key, st, dropFailed, err, logger)
	default:
		d.drop(key, st, dropRejected, err, logger)
	}
	st.transition(phaseIdle, logger)
	if st.pop(logger) {
		d.startHead(key, st, fx, logger)
	}
}

// drop reports the head as not delivered: a WARN, or a Debug when the runner
// is stopping.
func (d *dispatcher) drop(key string, st *notifierState, reason dropReason, err error, logger zerolog.Logger) {
	e := logger.Warn()
	if reason == dropStopped {
		e = logger.Debug()
	}
	e.Err(err).
		Str("event", string(st.head.Type)).
		Stringer("reason", reason).
		Int("attempts", st.attempt).
		Msg("Notification dropped")
	d.obs.dropped(key, st.head, reason, st.attempt)
}

// pop makes the oldest queued event the head with a fresh attempt count. With
// none left it closes any overflow episode and reports false.
func (st *notifierState) pop(logger zerolog.Logger) bool {
	st.head, st.attempt = Event{}, 0
	if len(st.queue) == 0 {
		if st.dropped > 0 {
			logger.Info().Int("dropped", st.dropped).Msg("Notification queue drained")
			st.dropped = 0
		}
		return false
	}
	st.head = st.queue[0]
	st.queue[0] = Event{}
	st.queue = st.queue[1:]
	return true
}
