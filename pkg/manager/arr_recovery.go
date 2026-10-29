package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// ArrRecovery is the stream-facing subset of the Arr service.
type ArrRecovery interface {
	Lookup(entryID, fileID string) (reacquire.Binding, bool)
	Reacquire(reacquire.Request) (*reacquire.Job, error)
	FailDownload(arrName, downloadID, entryID string) (*reacquire.Job, error)
	Jobs() []reacquire.Job
	Ready() bool
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

func (m *Manager) submitStreamReacquire(entryID, fileID, filename string) {
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
		if !m.streamFailureConfirmed(entryID, filename) {
			return
		}
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
		m.logger.Info().Str("entry_id", entryID).Str("file_id", fileID).Str("job", job.ID).
			Msg("Stream failed; queued an Arr reacquisition")
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
	if imported, err := m.reacquireDroppedFiles(provider, entry, recovery); imported {
		return err
	}

	record, err := m.Queue().GetTorrent(entry.InfoHash)
	if errors.Is(err, appendstore.ErrKeyNotFound) || (err == nil && record == nil) {
		m.logger.Info().Str("debrid", provider).Str("infohash", entry.InfoHash).Str("name", entry.Name).
			Msg("Debrid dropped a torrent with no queue record; nothing to recover")
		return nil
	}
	if err != nil {
		m.logger.Warn().Err(err).Str("infohash", entry.InfoHash).Msg("Failed to read queue record of dropped entry")
		return err
	}
	// Mark the record before queuing the job: the job removes the record once
	// the grab is failed, and a later write would bring it back.
	if record.State != storage.EntryStateError {
		record.MarkAsError(fmt.Errorf("%s no longer lists this torrent", provider))
		if updateErr := m.Queue().Update(record); updateErr != nil {
			return fmt.Errorf("mark dropped entry as errored: %w", updateErr)
		}
	}
	instance, ok := m.Arr().Get(record.Category)
	if !ok {
		return nil
	}
	if recovery == nil {
		return fmt.Errorf("arr recovery service is not running")
	}
	return m.failGrab(entry, recovery, instance.Name, provider+" dropped the torrent")
}

// reacquireDroppedFiles queues a reacquisition for each of a dropped entry's
// files the Arr imported. It reports whether any was imported.
func (m *Manager) reacquireDroppedFiles(provider string, entry *storage.Entry, recovery ArrRecovery) (bool, error) {
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
	if imported && reacquireErr == nil {
		m.logger.Info().Str("debrid", provider).Str("infohash", entry.InfoHash).Str("name", entry.Name).
			Msg("Debrid dropped an imported torrent; reacquiring its files")
	}
	return imported, reacquireErr
}

// failGrab queues the job that fails a never-imported download's grab in
// arrName. Sonarr and Radarr store qBittorrent download IDs uppercased.
func (m *Manager) failGrab(entry *storage.Entry, recovery ArrRecovery, arrName, reason string) error {
	job, err := recovery.FailDownload(arrName, strings.ToUpper(entry.InfoHash), entry.InfoHash)
	if err != nil {
		return fmt.Errorf("fail download in %s: %w", arrName, err)
	}
	event := m.logger.Info().Str("infohash", entry.InfoHash).Str("name", entry.Name).Str("arr", arrName)
	if job != nil {
		event = event.Str("job", job.ID)
	}
	event.Msg(reason + "; failing its grab in the Arr")
	return nil
}

// streamCheckTimeout bounds the full article check that confirms a stream failure before the file is replaced.
const streamCheckTimeout = 3 * time.Minute

// streamFailureConfirmed checks every article of the file, so a player blip or a scan never replaces a good file.
func (m *Manager) streamFailureConfirmed(entryID, filename string) bool {
	check := m.checkStreamFile
	if check == nil {
		if m.usenet == nil {
			return false
		}
		check = m.usenet.CheckWholeFile
	}
	ctx, cancel := context.WithTimeout(context.Background(), streamCheckTimeout)
	defer cancel()
	err := check(ctx, entryID, filename)
	if errors.Is(err, customerror.ErrUsenetSegmentMissing) || errors.Is(err, customerror.ErrUsenetManifestMissing) ||
		errors.Is(err, customerror.ErrUsenetManifestInvalid) {
		return true
	}
	event := m.logger.Info().Err(err).Str("entry_id", entryID).Str("file", filename)
	if ctx.Err() != nil {
		event.Msg("Stream failed but the article check did not finish; not reacquiring")
		return false
	}
	event.Msg("Stream failed but a full article check found nothing missing; not reacquiring")
	return false
}

// refreshAfterSave shows a changed bad mark in the library: entries hidden or listed by Bad are rebuilt.
func (m *Manager) refreshAfterSave() {
	m.InvalidateEntryCache()
	if err := m.RefreshMount(); err != nil {
		m.logger.Error().Err(err).Msg("Mount refresh failed")
	}
}

// ClearBad lifts a bad mark that a fresh read check disproved; a repair in flight defers it to the next sweep.
func (m *Manager) ClearBad(_ context.Context, infohash string) error {
	entry, err := m.GetEntry(infohash)
	if errors.Is(err, appendstore.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !entry.Bad {
		return nil
	}
	if _, inFlight := m.fixer.inFlightRepairs.Load(infohash); inFlight {
		return m.storage.MarkEntryDirty(entry.GetFolder(m.folderNaming()), config.ProtocolTorrent, "marked_bad")
	}
	entry.Bad = false
	entry.UpdatedAt = time.Now()
	if err = m.AddOrUpdate(entry, func(*storage.Entry) { m.refreshAfterSave() }); err != nil {
		return err
	}
	m.fixer.ResetFailureState(infohash)
	m.logger.Info().Str("infohash", infohash).Str("name", entry.Name).Msg("cleared bad mark: files pass the read check")
	return nil
}

// confirmCheckTimeout bounds each fresh read check of an entry's active placement.
const confirmCheckTimeout = time.Minute

// entryBroken reports whether the entry's active placement fails a fresh read check.
func (m *Manager) entryBroken(entry *storage.Entry) bool {
	ctx, cancel := context.WithTimeout(context.Background(), confirmCheckTimeout)
	defer cancel()
	if m.confirmBroken != nil {
		return m.confirmBroken(ctx, entry)
	}
	return m.repair.ConfirmBroken(ctx, entry)
}

// giveUpOnEntry marks a confirmed-broken entry bad; the arr grab waits for a second check, as a blocklist is final.
func (m *Manager) giveUpOnEntry(entry *storage.Entry) {
	if !m.entryBroken(entry) {
		m.logger.Warn().Str("infohash", entry.InfoHash).Str("name", entry.Name).
			Msg("not marking bad: active placement still serves links or the check was inconclusive")
		return
	}
	entry.Bad = true
	if err := m.AddOrUpdate(entry, func(*storage.Entry) { m.refreshAfterSave() }); err != nil {
		m.logger.Warn().Err(err).Str("infohash", entry.InfoHash).Msg("Failed to persist the bad flag")
	}
	if entry.IsTorrent() {
		if err := m.storage.MarkEntryDirty(entry.GetFolder(m.folderNaming()), config.ProtocolTorrent,
			"marked_bad"); err != nil {
			m.logger.Warn().Err(err).Str("infohash", entry.InfoHash).Msg("Failed to queue the bad entry for a recheck")
		}
	}
	go m.recheckBadEntry(entry.InfoHash)
}

// recheckBadEntry decides the arr grab of a freshly marked entry after badRecheckDelay.
func (m *Manager) recheckBadEntry(infohash string) {
	select {
	case <-time.After(m.badRecheckDelay):
	case <-m.ctx.Done():
		return
	}
	entry, err := m.GetEntry(infohash)
	if err != nil || entry == nil || !entry.Bad {
		return
	}
	if !m.entryBroken(entry) {
		if clearErr := m.ClearBad(m.ctx, infohash); clearErr != nil {
			m.logger.Warn().Err(clearErr).Str("infohash", infohash).Msg("Failed to clear a disproved bad mark")
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	m.failGrabIfQueued(ctx, entry)
}

// failGrabIfQueued fails an arr torrent's grab only while the arr still queues it; imported files stay with repair.
func (m *Manager) failGrabIfQueued(ctx context.Context, entry *storage.Entry) {
	if !entry.IsTorrent() || entry.Manual {
		return
	}
	instance, ok := m.Arr().Get(entry.Category)
	recovery := m.recoveryService()
	if !ok || recovery == nil {
		return
	}
	records, err := m.Arr().Queue(ctx, instance.Name)
	if err != nil {
		m.logger.Warn().Err(err).Str("arr", instance.Name).Str("infohash", entry.InfoHash).
			Msg("Arr queue unreadable; leaving the grab of a bad entry")
		return
	}
	queued := slices.ContainsFunc(records, func(record arr.QueueSchema) bool {
		return strings.EqualFold(record.DownloadID, entry.InfoHash)
	})
	if !queued {
		return
	}
	if grabErr := m.failGrab(entry, recovery, instance.Name, "No provider can serve the torrent"); grabErr != nil {
		m.logger.Warn().Err(grabErr).Str("name", entry.Name).Msg("Failed to fail the grab in the Arr")
	}
}
