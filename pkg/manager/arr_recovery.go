package manager

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// ArrRecovery is the stream-facing subset of the Arr service.
type ArrRecovery interface {
	Lookup(entryID, fileID string) (reacquire.Binding, bool)
	Reacquire(reacquire.Request) (*reacquire.Job, error)
	FailDownload(arrName, downloadID, entryID string) (*reacquire.Job, error)
}

var _ ArrRecovery = (*reacquire.Service)(nil)

type streamTarget struct {
	entryID string
	fileID  string
}

// SetArrRecovery installs the Arr recovery service used by streaming failures.
func (m *Manager) SetArrRecovery(recovery ArrRecovery) {
	m.arrRecoveryMu.Lock()
	m.arrRecovery = recovery
	m.arrRecoveryMu.Unlock()
}

func (m *Manager) recoveryService() ArrRecovery {
	m.arrRecoveryMu.RLock()
	recovery := m.arrRecovery
	m.arrRecoveryMu.RUnlock()
	return recovery
}

func (m *Manager) lookupArrBinding(entryID, fileID string) (reacquire.Binding, bool) {
	recovery := m.recoveryService()
	if recovery == nil || entryID == "" || fileID == "" {
		return reacquire.Binding{}, false
	}
	binding, ok := recovery.Lookup(entryID, fileID)
	if ok {
		binding.EpisodeIDs = slices.Clone(binding.EpisodeIDs)
	}
	return binding, ok
}

func (m *Manager) submitStreamReacquire(entryID, fileID string) {
	recovery := m.recoveryService()
	if recovery == nil || entryID == "" || fileID == "" {
		return
	}
	// Files can be unclaimed or still waiting for the Arr index.
	if _, ok := recovery.Lookup(entryID, fileID); !ok {
		return
	}

	target := streamTarget{entryID: entryID, fileID: fileID}
	if _, loaded := m.reacquireNotifications.LoadOrStore(target, struct{}{}); loaded {
		return
	}

	go func() {
		defer m.reacquireNotifications.Delete(target)
		job, err := recovery.Reacquire(reacquire.Request{
			EntryID: entryID,
			FileID:  fileID,
			Cause:   reacquire.CauseStream,
		})
		if err != nil {
			if errors.Is(err, reacquire.ErrBindingNotFound) {
				return
			}
			m.logger.Error().Err(err).
				Str("entry_id", entryID).
				Str("file_id", fileID).
				Msg("Failed to queue Arr reacquisition")
			return
		}
		if job == nil {
			return
		}
		m.setStreamReacquireJob(entryID, fileID, job.ID)
	}()
}

func (m *Manager) setStreamReacquireJob(entryID, fileID, jobID string) {
	if jobID == "" || m.activeStreams == nil {
		return
	}
	m.activeStreams.Range(func(_ string, stream *ActiveStream) bool {
		if stream.EntryID == entryID && stream.FileID == fileID {
			stream.setReacquireJobID(jobID)
		}
		return true
	})
}

// recoverDroppedEntry makes the Arr react to an entry whose debrid placement
// was confirmed gone. Imported files are reacquired; a download the Arr is
// still tracking has its grab failed, so the Arr blocklists the release
// instead of retrying a dangling import forever. An error means recovery is
// incomplete and must be retried; every step is safe to repeat.
func (m *Manager) recoverDroppedEntry(provider string, entry *storage.Entry) error {
	// Read the service once: SetArrRecovery(nil) may run at shutdown.
	recovery := m.recoveryService()
	imported := false
	var reacquireErr error
	for _, file := range entry.Files {
		if recovery == nil || entry.InfoHash == "" || file.ID == "" {
			continue
		}
		if _, ok := recovery.Lookup(entry.InfoHash, file.ID); !ok {
			continue
		}
		imported = true
		if _, err := recovery.Reacquire(reacquire.Request{
			EntryID: entry.InfoHash,
			FileID:  file.ID,
			Cause:   reacquire.CauseRepair,
		}); err != nil {
			reacquireErr = errors.Join(reacquireErr, fmt.Errorf("reacquire file %s: %w", file.ID, err))
		}
	}
	if imported {
		return reacquireErr
	}

	record, err := m.Queue().GetTorrent(entry.InfoHash)
	if errors.Is(err, appendstore.ErrKeyNotFound) || (err == nil && record == nil) {
		return nil
	}
	if err != nil {
		m.logger.Warn().Err(err).Str("infohash", entry.InfoHash).Msg("Failed to read queue record of dropped entry")
		return err
	}
	if instance, ok := m.Arr().Get(record.Category); ok {
		if recovery == nil {
			return fmt.Errorf("arr recovery service is not running")
		}
		downloadID := strings.ToUpper(entry.InfoHash)
		if _, failErr := recovery.FailDownload(instance.Name, downloadID, entry.InfoHash); failErr != nil {
			return fmt.Errorf("fail dropped download in %s: %w", instance.Name, failErr)
		}
	}
	if record.State == storage.EntryStateError {
		return nil
	}
	record.MarkAsError(fmt.Errorf("%s no longer lists this torrent", provider))
	if updateErr := m.Queue().Update(record); updateErr != nil {
		return fmt.Errorf("mark dropped entry as errored: %w", updateErr)
	}
	return nil
}
