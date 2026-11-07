package manager

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/sirrobot01/decypharr/pkg/arr"
)

// heldGrab is the grab failure of a never-imported torrent that the arr blocklist guard held back. The paths that
// fail grabs (a rejected release, a bad-mark recheck) do not retry themselves, so the manager keeps the work and
// retries it, oldest first. The list is in memory: a restart loses it, and the entry in error is then judged again
// by whatever failed it.
type heldGrab struct {
	infoHash string
	retry    func(ctx context.Context) error
}

type heldGrabs struct {
	mu   sync.Mutex
	list []heldGrab
}

// holdGrab keeps a held grab failure for a later retry, once per torrent.
func (m *Manager) holdGrab(infoHash string, retry func(ctx context.Context) error) {
	m.heldGrabs.mu.Lock()
	defer m.heldGrabs.mu.Unlock()
	if slices.ContainsFunc(m.heldGrabs.list, func(held heldGrab) bool { return held.infoHash == infoHash }) {
		return
	}
	m.heldGrabs.list = append(m.heldGrabs.list, heldGrab{infoHash: infoHash, retry: retry})
}

func (m *Manager) heldGrabCount() int {
	m.heldGrabs.mu.Lock()
	defer m.heldGrabs.mu.Unlock()
	return len(m.heldGrabs.list)
}

// retryHeldGrabs retries the held grab failures oldest first. It stops at the first the guard still holds: the
// rest share its bucket or the arr's cap, and retrying them would only ask the canary again.
func (m *Manager) retryHeldGrabs(ctx context.Context) {
	for {
		m.heldGrabs.mu.Lock()
		if len(m.heldGrabs.list) == 0 || ctx.Err() != nil {
			m.heldGrabs.mu.Unlock()
			return
		}
		oldest := m.heldGrabs.list[0]
		m.heldGrabs.mu.Unlock()

		err := oldest.retry(ctx)
		if errors.Is(err, arr.ErrBlocklistHeld) {
			return
		}
		if err != nil {
			m.logger.Warn().
				Err(err).
				Str("infohash", oldest.infoHash).
				Msg("Failed to fail a held grab in the Arr; dropping it")
		}
		m.heldGrabs.mu.Lock()
		if len(m.heldGrabs.list) > 0 && m.heldGrabs.list[0].infoHash == oldest.infoHash {
			m.heldGrabs.list = m.heldGrabs.list[1:]
		}
		m.heldGrabs.mu.Unlock()
	}
}
