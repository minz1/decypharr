package manager

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// maxHeldGrabAttempts bounds how often a held grab is retried when the retry itself fails (not when the guard holds
// it again), so one that can never succeed does not stay forever.
const maxHeldGrabAttempts = 5

// heldGrab is the grab failure of a never-imported torrent that the arr blocklist guard held back. The paths that
// fail grabs (a rejected release, a bad-mark recheck) do not retry themselves, so the manager keeps the work and
// retries it, oldest first. The list is in memory: a restart loses it, and the entry in error is then judged again
// by whatever failed it.
type heldGrab struct {
	infoHash string
	// bucket names the arr and provider the guard judged it in; grabs in the same bucket wait on each other.
	bucket   string
	attempts int
	retry    func(ctx context.Context) error
}

type heldGrabs struct {
	mu   sync.Mutex
	list []heldGrab
}

// holdGrab keeps a held grab failure for a later retry, once per torrent.
func (m *Manager) holdGrab(infoHash, bucket string, retry func(ctx context.Context) error) {
	m.heldGrabs.mu.Lock()
	defer m.heldGrabs.mu.Unlock()
	if slices.ContainsFunc(m.heldGrabs.list, func(held heldGrab) bool { return held.infoHash == infoHash }) {
		return
	}
	m.heldGrabs.list = append(m.heldGrabs.list, heldGrab{infoHash: infoHash, bucket: bucket, retry: retry})
}

// grabBucket names the bucket the guard judges a grab failure in: its arr and the entry's provider.
func grabBucket(entry *storage.Entry) string {
	provider := entry.ActiveProvider
	if provider == "" {
		provider = string(config.ProtocolTorrent)
	}
	return entry.Category + "/" + provider
}

func (m *Manager) heldGrabCount() int {
	m.heldGrabs.mu.Lock()
	defer m.heldGrabs.mu.Unlock()
	return len(m.heldGrabs.list)
}

// retryHeldGrabs retries the held grab failures oldest first. Once the guard holds one again, the rest of its
// bucket is left for the next pass, since they share its limits and canary; other buckets carry on. A retry that
// fails for another reason, such as an unreadable Arr queue, keeps the grab for a bounded number of tries.
func (m *Manager) retryHeldGrabs(ctx context.Context) {
	m.heldGrabs.mu.Lock()
	pending := slices.Clone(m.heldGrabs.list)
	m.heldGrabs.mu.Unlock()

	waiting := make(map[string]bool)
	for _, held := range pending {
		if ctx.Err() != nil {
			return
		}
		if waiting[held.bucket] {
			continue
		}
		err := held.retry(ctx)
		switch {
		case errors.Is(err, arr.ErrBlocklistHeld):
			waiting[held.bucket] = true
		case err != nil:
			m.noteHeldGrabFailure(held, err)
		default:
			m.dropHeldGrab(held.infoHash)
		}
	}
}

func (m *Manager) noteHeldGrabFailure(held heldGrab, err error) {
	m.heldGrabs.mu.Lock()
	defer m.heldGrabs.mu.Unlock()
	i := slices.IndexFunc(m.heldGrabs.list, func(item heldGrab) bool { return item.infoHash == held.infoHash })
	if i < 0 {
		return
	}
	m.heldGrabs.list[i].attempts++
	if m.heldGrabs.list[i].attempts < maxHeldGrabAttempts {
		return
	}
	m.logger.Warn().Err(err).Str("infohash", held.infoHash).Int("attempts", maxHeldGrabAttempts).
		Msg("Failed to fail a held grab in the Arr; giving up on it")
	m.heldGrabs.list = slices.Delete(m.heldGrabs.list, i, i+1)
}

func (m *Manager) dropHeldGrab(infoHash string) {
	m.heldGrabs.mu.Lock()
	defer m.heldGrabs.mu.Unlock()
	m.heldGrabs.list = slices.DeleteFunc(
		m.heldGrabs.list,
		func(held heldGrab) bool { return held.infoHash == infoHash },
	)
}
