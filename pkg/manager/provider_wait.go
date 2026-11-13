package manager

import (
	"errors"
	"strconv"
	"strings"
	"sync"

	"github.com/sirrobot01/decypharr/internal/customerror"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// providerWait is what a queued entry is waiting on at its provider, kept in memory so a wait is logged when it
// starts or changes rather than on every queue pass.
type providerWait uint8

const (
	// waitNone is an entry whose last status check was not a provider-pending wait.
	waitNone providerWait = iota
	// waitConverting is a magnet the provider has not converted yet.
	waitConverting
	// waitPending is any other provider-pending wait, such as links not visible yet.
	waitPending
)

func (w providerWait) String() string {
	switch w {
	case waitNone:
		return "no wait"
	case waitConverting:
		return "converting"
	case waitPending:
		return "pending"
	}
	return "providerWait(" + strconv.Itoa(int(w)) + ")"
}

// providerWaitOf classifies a status-check error: waitNone for anything that is not a provider-pending wait.
func providerWaitOf(err error) providerWait {
	switch {
	case errors.Is(err, customerror.ErrMagnetNotConverted):
		return waitConverting
	case errors.Is(err, customerror.ErrProviderStatusTimeout):
		return waitPending
	default:
		return waitNone
	}
}

// providerWaits holds each queued entry's current wait, keyed by lower-case infohash as the queue store is. The zero
// value is ready to use.
type providerWaits struct {
	mu   sync.Mutex
	byID map[string]providerWait
}

// observe records the wait of the entry's latest status check and reports the wait it replaced. Every wait may
// follow every other, so there is no illegal move; waitNone forgets the entry.
func (w *providerWaits) observe(infohash string, next providerWait) providerWait {
	key := strings.ToLower(infohash)
	w.mu.Lock()
	defer w.mu.Unlock()
	previous := w.byID[key]
	if next == waitNone {
		delete(w.byID, key)
		return previous
	}
	if w.byID == nil {
		w.byID = make(map[string]providerWait)
	}
	w.byID[key] = next
	return previous
}

func providerWaitOfPhase(phase debridTypes.Phase) providerWait {
	switch phase {
	case debridTypes.PhaseConverting:
		return waitConverting
	case debridTypes.PhaseFinalizing:
		return waitPending
	case debridTypes.PhaseNone, debridTypes.PhaseUnknown, debridTypes.PhaseAwaitingSelection,
		debridTypes.PhaseRemoteQueued, debridTypes.PhaseDownloading, debridTypes.PhaseStalled,
		debridTypes.PhaseDownloaded, debridTypes.PhaseFailed:
	}
	return waitNone
}
