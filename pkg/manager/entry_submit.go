package manager

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/task/keyed"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// submitEntry hands a new grab to its actor. A key whose grab is already in hand takes no second submit.
func (m *Manager) submitEntry(ctx context.Context, entry *storage.Entry, req *ImportRequest) error {
	msg := eventMsg(grab.Event{Kind: grab.EvSubmit})
	msg.entry, msg.req = entry, req
	v, err := m.entries.runner.Ask(ctx, strings.ToLower(entry.InfoHash), msg)
	if err == nil && v != nil {
		err, _ = v.(error)
	}
	return addError(err)
}

// addError maps what the actor said about an add to the error the add's caller answers with.
func addError(err error) error {
	refused, isRefused := errors.AsType[*grab.RefusedError](err)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, keyed.ErrStopped), errors.Is(err, keyed.ErrMailboxFull):
		return customerror.NewError(fmt.Errorf("not added, try again: %w", err), http.StatusServiceUnavailable,
			"unavailable", false, false).Retryable()
	case isRefused && refused.Phase == grab.PhaseDeleting:
		return customerror.NewError(fmt.Errorf("being deleted, try again: %w", err), http.StatusServiceUnavailable,
			"busy", false, false).Retryable()
	case isRefused:
		return customerror.NewError(fmt.Errorf("not added: %w", err), http.StatusConflict, "refused", false, false).
			Permanent()
	}
	return err
}

// mayHoldRecord reports an add whose record the grab may hold: it succeeded, or its outcome is unknown.
func mayHoldRecord(err error) bool {
	_, refused := errors.AsType[*grab.RefusedError](err)
	if err == nil {
		return true
	}
	return !refused && !errors.Is(err, keyed.ErrStopped) && !errors.Is(err, keyed.ErrMailboxFull) &&
		!errors.Is(err, keyed.ErrNotSent)
}

// readQueue reads every queue record for recovery and the season-pack index of them. A record that does not
// read is logged and left out; the rest are recovered.
func (m *Manager) readQueue() ([]*storage.Entry, map[string]string) {
	records, err := m.storage.FilterQueuedGrab(nil)
	if err != nil {
		m.logger.Error().Err(err).Int("read", len(records)).
			Msg("Some queued downloads could not be read; the rest are recovered")
	}
	hashes := make([]string, 0, len(records))
	for _, e := range records {
		hashes = append(hashes, e.InfoHash)
	}
	return records, grab.SeasonIndex(hashes)
}

// recoverEntries shows every queue record to readers and sends Recover to each grab still in flight, oldest
// first. A refused Send is logged; the grab is recovered on the next start.
func (m *Manager) recoverEntries(records []*storage.Entry, packOf map[string]string) {
	type pending struct {
		key string
		g   grab.Grab
	}
	var inFlight []pending
	for _, e := range records {
		key := strings.ToLower(e.InfoHash)
		g, err := grab.Load(e, packOf, m.entries.inLibrary(key))
		if err != nil {
			m.logger.Warn().Err(err).Str("key", key).Msg("Grab document unreadable; using the state its record implies")
		}
		m.grabs.publish(GrabView{State: g.State, Entry: *g.Entry})
		if m.grabs.active(key) {
			inFlight = append(inFlight, pending{key: key, g: g})
		}
	}
	slices.SortStableFunc(inFlight, func(a, b pending) int { return a.g.Entry.AddedOn.Compare(b.g.Entry.AddedOn) })
	refused := 0
	for _, p := range inFlight {
		if err := m.entries.runner.Send(p.key, eventMsg(grab.Event{Kind: grab.EvRecover})); err != nil {
			refused++
			m.logger.Warn().Err(err).Str("key", p.key).Msg("Grab not recovered; the next start recovers it")
		}
	}
	if refused > 0 {
		m.logger.Warn().Int("refused", refused).Int("of", len(inFlight)).Msg("Some grabs were not recovered")
	}
}
