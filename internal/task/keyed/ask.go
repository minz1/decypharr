package keyed

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	// ErrAskInHandler is returned by Ask when it is called from a handler or from a child task of one: the
	// handler would wait for an actor that may be itself. Actors talk with Effects.Send.
	ErrAskInHandler = errors.New("keyed: Ask called from inside a handler")
	// ErrOutcomeUnknown wraps the error of an Ask that gave up, or whose message was dropped, after the message
	// was queued: it may have been handled, or may be. The command must be safe to repeat.
	ErrOutcomeUnknown = errors.New("keyed: the outcome of the message is unknown")
	// ErrNotSent wraps the error of an Ask whose context had ended before it was called: nothing was queued, so
	// the outcome is known, unlike ErrOutcomeUnknown.
	ErrNotSent = errors.New("keyed: the message was not sent")
	// ErrNoReply is the reply of an Ask whose handler returned without calling Effects.Reply.
	ErrNoReply = errors.New("keyed: the handler did not reply")
	// ErrHandlerPanicked is the reply of an Ask whose handler panicked, or whose message was queued for a key whose
	// Activate panicked: the message was never handled, since there was no state to handle it on.
	ErrHandlerPanicked = errors.New("keyed: the handler panicked")
)

// replySlot carries the one reply of an Ask.
type replySlot struct {
	once sync.Once
	ch   chan askResult
}

type askResult struct {
	value any
	err   error
}

func newReplySlot() *replySlot { return &replySlot{ch: make(chan askResult, 1)} }

// finish delivers the reply once; later calls do nothing. It never blocks.
func (s *replySlot) finish(value any, err error) {
	s.once.Do(func() { s.ch <- askResult{value: value, err: err} })
}

// Ask sends msg to key and waits for the handler's reply, until ctx ends. It
// is for callers outside the actors, such as an API handler. A timeout means
// the outcome is unknown, not that the message failed: the error wraps
// ErrOutcomeUnknown and the actor may still handle the message, so the
// command must be safe to repeat. Ask returns ErrStopped for a stopped
// runner and ErrMailboxFull when the key has no room, in both cases without
// having queued anything. Called from a handler it returns ErrAskInHandler.
func (r *Runner[K, M, S]) Ask(ctx context.Context, key K, msg M) (any, error) {
	if ctx.Value(inHandler{}) != nil {
		return nil, ErrAskInHandler
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotSent, context.Cause(ctx))
	}
	slot := newReplySlot()
	r.mu.Lock()
	err := r.enqueueLocked(key, envelope[M]{msg: msg, at: time.Now(), class: Command, reply: slot})
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case result := <-slot.ch:
		return result.value, result.err
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrOutcomeUnknown, context.Cause(ctx))
	}
}

// Reply sets the reply of the Ask that sent the message being handled; it is
// delivered when Handle returns, so the handler has finished changing its
// state by then. A message that was not sent with Ask ignores it, and so does
// every call after the first.
//
// Reply works only inside Handle, so a command cannot answer after the I/O of
// a child task: the actor answers with what it knows (accepted, in progress)
// and reports the outcome some other way. An Ask whose answer depends on
// child I/O gets the "unknown" error at its timeout, and the caller (an API
// handler returning 503) retries; commands must be idempotent for that.
func (fx *Effects[K, M]) Reply(value any) {
	fx.live()
	if fx.replied {
		return
	}
	fx.replied, fx.value = true, value
}

// finishReply gives the asker the handler's reply, or the error that
// stands in for it.
func (fx *Effects[K, M]) finishReply(slot *replySlot, panicked bool) {
	if slot == nil {
		return
	}
	switch {
	case panicked:
		slot.finish(nil, ErrHandlerPanicked)
	case fx.replied:
		slot.finish(fx.value, nil)
	default:
		slot.finish(nil, ErrNoReply)
	}
}

// dropReply tells the asker of a queued message that the runner stopped before it was handled.
func dropReply(slot *replySlot) {
	if slot != nil {
		slot.finish(nil, fmt.Errorf("%w: %w", ErrOutcomeUnknown, ErrStopped))
	}
}
