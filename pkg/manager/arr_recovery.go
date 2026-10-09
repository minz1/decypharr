package manager

import (
	"fmt"
	"slices"
	"strings"

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
// still tracking is marked errored and its grab failed, so the Arr blocklists
// the release instead of retrying a dangling import forever.
func (m *Manager) recoverDroppedEntry(provider string, entry *storage.Entry) {
	recovery := m.recoveryService()
	imported := false
	if recovery != nil {
		for _, file := range entry.Files {
			if _, ok := m.lookupArrBinding(entry.InfoHash, file.ID); !ok {
				continue
			}
			imported = true
			if _, err := recovery.Reacquire(reacquire.Request{
				EntryID: entry.InfoHash,
				FileID:  file.ID,
				Cause:   reacquire.CauseRepair,
			}); err != nil {
				m.logger.Error().Err(err).Str("infohash", entry.InfoHash).Str("file_id", file.ID).
					Msg("Failed to queue Arr reacquisition for dropped entry")
			}
		}
	}
	if imported {
		return
	}

	record, err := m.Queue().GetTorrent(entry.InfoHash)
	if err != nil || record == nil || record.State == storage.EntryStateError {
		return
	}
	record.MarkAsError(fmt.Errorf("%s no longer lists this torrent", provider))
	if updateErr := m.Queue().Update(record); updateErr != nil {
		m.logger.Error().Err(updateErr).Str("infohash", entry.InfoHash).Msg("Failed to mark dropped entry as errored")
	}
	if recovery == nil {
		return
	}
	instance, ok := m.Arr().Get(record.Category)
	if !ok {
		return
	}
	downloadID := strings.ToUpper(entry.InfoHash)
	if _, failErr := recovery.FailDownload(instance.Name, downloadID, entry.InfoHash); failErr != nil {
		m.logger.Error().Err(failErr).Str("infohash", entry.InfoHash).Str("arr", instance.Name).
			Msg("Failed to fail dropped download in Arr")
	}
}
