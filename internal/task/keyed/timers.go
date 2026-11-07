package keyed

import (
	"container/heap"
	"context"
	"fmt"
	"slices"
	"time"
)

// TimerID names a timer set with After. The zero value is no timer.
type TimerID uint64

// timerEntry is a pending timer.
type timerEntry[K comparable, M any] struct {
	id    TimerID
	key   K
	msg   M
	due   time.Time
	index int // position in the heap
}

// timerHeap orders timers by due time, ties by the order they were set.
type timerHeap[K comparable, M any] []*timerEntry[K, M]

func (h timerHeap[K, M]) Len() int { return len(h) }
func (h timerHeap[K, M]) Less(i, j int) bool {
	if h[i].due.Equal(h[j].due) {
		return h[i].id < h[j].id
	}
	return h[i].due.Before(h[j].due)
}
func (h timerHeap[K, M]) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *timerHeap[K, M]) Push(x any) {
	entry, _ := x.(*timerEntry[K, M])
	entry.index = len(*h)
	*h = append(*h, entry)
}
func (h *timerHeap[K, M]) Pop() any {
	old := *h
	n := len(old)
	entry := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return entry
}

// After delivers msg to key once d has passed. The timer belongs to the
// runner, not to the key's goroutine, so the goroutine's idle exit does not
// lose it: the message goes through the mailbox, past its bound, and starts
// the goroutine if it has exited. It returns the timer's id, or 0 once the
// runner has stopped.
func (r *Runner[K, M, S]) After(key K, d time.Duration, msg M) TimerID {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.accepting() {
		return 0
	}
	r.lastTimer++
	entry := &timerEntry[K, M]{id: r.lastTimer, key: key, msg: msg, due: time.Now().Add(d)}
	heap.Push(&r.timers, entry)
	r.timerByID[entry.id] = entry
	r.actorLocked(key).timers++
	select {
	case r.timerWake <- struct{}{}:
	default:
	}
	return entry.id
}

// CancelTimer cancels a timer. A timer that has not fired is removed; one that
// fired but whose message still waits in the mailbox has the message taken
// back out, so a cancelled deadline is never handled. A message the actor is
// already handling cannot be recalled, and a timer already cancelled is left
// alone.
func (r *Runner[K, M, S]) CancelTimer(id TimerID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.timerByID[id]
	if !ok {
		r.recallQueuedLocked(id)
		return
	}
	heap.Remove(&r.timers, entry.index)
	delete(r.timerByID, id)
	if a, found := r.keys[entry.key]; found {
		a.timers--
		r.dropIfIdleLocked(a)
	}
}

// recallQueuedLocked removes the message of a fired timer from its mailbox, if it still waits there.
func (r *Runner[K, M, S]) recallQueuedLocked(id TimerID) {
	key, ok := r.queuedTimers[id]
	if !ok {
		return
	}
	delete(r.queuedTimers, id)
	a, found := r.keys[key]
	if !found {
		return
	}
	if i := slices.IndexFunc(a.queue, func(env envelope[M]) bool { return env.timer == id }); i >= 0 {
		a.queue = slices.Delete(a.queue, i, i+1)
		a.bypassed--
	}
	r.dropIfIdleLocked(a)
}

// fireDueLocked delivers every timer that is due.
func (r *Runner[K, M, S]) fireDueLocked(now time.Time) {
	for len(r.timers) > 0 && !r.timers[0].due.After(now) {
		entry, _ := heap.Pop(&r.timers).(*timerEntry[K, M])
		delete(r.timerByID, entry.id)
		a := r.actorLocked(entry.key)
		a.timers--
		if err := r.enqueueLocked(
			entry.key,
			envelope[M]{msg: entry.msg, at: now, bypass: true, timer: entry.id},
		); err != nil {
			r.logger.Debug().Err(err).Str("runner", r.name).Str("key", fmt.Sprint(entry.key)).
				Msg("A timer fired on a stopping runner and was dropped")
			continue
		}
		r.queuedTimers[entry.id] = entry.key
	}
}

// clearTimersLocked cancels every pending timer.
func (r *Runner[K, M, S]) clearTimersLocked() {
	r.timers = nil
	clear(r.timerByID)
	for _, a := range r.keys {
		a.timers = 0
		r.dropIfIdleLocked(a)
	}
}

// supervise is the runner's own task: it fires the timers as they fall due,
// and ends the runner when the group stops.
func (r *Runner[K, M, S]) supervise(ctx context.Context) error {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		r.mu.Lock()
		r.fireDueLocked(time.Now())
		var due <-chan time.Time
		if len(r.timers) > 0 {
			timer.Reset(time.Until(r.timers[0].due))
			due = timer.C
		} else {
			timer.Stop()
		}
		r.mu.Unlock()

		select {
		case <-ctx.Done():
			r.beginStop()
			return nil
		case <-r.timerWake:
		case <-due:
		}
	}
}

// After sets a timer that delivers msg to this key. See [Runner.After].
func (fx *Effects[K, M]) After(d time.Duration, msg M) TimerID {
	return fx.r.After(fx.key, d, msg)
}

// CancelTimer cancels a timer of this runner. See [Runner.CancelTimer].
func (fx *Effects[K, M]) CancelTimer(id TimerID) {
	fx.r.CancelTimer(id)
}
