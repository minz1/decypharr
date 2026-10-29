package repair

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

var (
	errReacquirerUnavailable = errors.New("arr reacquirer is unavailable")
	// errUnmappedBrokenFile marks a broken file the Arr service cannot act on
	// because it has no stable managed identity to bind to.
	errUnmappedBrokenFile = errors.New("broken file has no managed identity")
)

func (r *Service) repairBroken(
	ctx context.Context,
	run *storage.RepairRun,
	healths *xsync.Map[string, *storage.EntryHealth],
) {
	var statsMu sync.Mutex
	healths.Range(func(_ string, health *storage.EntryHealth) bool {
		if ctx != nil && ctx.Err() != nil {
			return false
		}
		r.healBrokenEntry(ctx, run, &statsMu, health, health.BrokenFiles)
		return true
	})
}

func (r *Service) healBrokenEntry(
	ctx context.Context,
	run *storage.RepairRun,
	statsMu *sync.Mutex,
	health *storage.EntryHealth,
	files []storage.BrokenFile,
) []storage.BrokenFile {
	if health == nil || health.Status != storage.HealthBroken {
		return nil
	}

	blocklist := blocklistedReleases(files)
	initiated := 0
	failed := 0
	var unhealed []storage.BrokenFile
	for i, broken := range files {
		if ctx != nil && ctx.Err() != nil {
			unhealed = files[i:]
			break
		}
		if broken.ArrName == "" || !r.reacquirable(broken) {
			continue
		}
		strategy := reacquire.StrategyCommandSearch
		if blocklist[broken.InfoHash] {
			strategy = reacquire.StrategyHistoryFailed
		}
		job, err := r.reacquireBrokenFile(ctx, broken, strategy)
		if err != nil && ctx != nil && ctx.Err() != nil {
			unhealed = files[i:]
			break
		}
		r.logReacquire(broken, job, err)
		if err != nil {
			failed++
			continue
		}
		initiated++
	}

	if initiated == 0 && failed == 0 {
		return unhealed
	}
	health.LastRepairAt = time.Now()
	r.saveHealth(health)
	statsMu.Lock()
	run.Stats.Repaired += initiated
	run.Stats.RepairFailed += failed
	r.saveRun(run)
	statsMu.Unlock()
	return unhealed
}

// blocklistedReleases returns the InfoHashes with a release-content failure among files.
func blocklistedReleases(files []storage.BrokenFile) map[string]bool {
	blocklist := make(map[string]bool)
	for _, broken := range files {
		if strategyForReason(broken.Reason) == reacquire.StrategyHistoryFailed {
			blocklist[broken.InfoHash] = true
		}
	}
	return blocklist
}

func (r *Service) logReacquire(broken storage.BrokenFile, job *reacquire.Job, err error) {
	if err != nil {
		r.logger.Warn().Err(err).
			Str("arr", broken.ArrName).
			Str("entry_id", broken.InfoHash).
			Str("file", broken.FileName).
			Msg("Repair: failed to queue Arr reacquisition")
		return
	}
	r.logger.Info().
		Str("arr", broken.ArrName).
		Str("entry_id", broken.InfoHash).
		Str("file", broken.FileName).
		Str("job_id", job.ID).
		Msg("Repair: queued Arr reacquisition")
}

// Reasons for a failure in the release's own content.
const (
	reasonHosterUnavailable    = "hoster_unavailable"
	reasonEmptyDownloadLink    = "empty_download_link"
	reasonUsenetSegmentMissing = "usenet_segment_missing"
	reasonUsenetCorruptContent = "usenet_corrupt_content"
)

// reasonProviderClientNotFound is a provider missing from the manager: decypharr's side, never the release's.
const reasonProviderClientNotFound = "provider_client_not_found"

func strategyForReason(reason string) reacquire.Strategy {
	switch reason {
	case reasonHosterUnavailable, reasonEmptyDownloadLink, reasonUsenetSegmentMissing, reasonUsenetCorruptContent:
		return reacquire.StrategyHistoryFailed
	default:
		return reacquire.StrategyCommandSearch
	}
}

func (r *Service) arrInstance(name string) (arr.Arr, bool) {
	if r.arrs == nil {
		return arr.Arr{}, false
	}
	return r.arrs.Get(name)
}

// reacquirable reports whether the owning Arr can run a reacquisition. Only
// Sonarr and Radarr expose the file, history, and search APIs the Arr service
// drives; every other kind is left untouched instead of failing every sweep.
func (r *Service) reacquirable(broken storage.BrokenFile) bool {
	kind := broken.ArrKind
	if instance, ok := r.arrInstance(broken.ArrName); ok {
		kind = arrKindFromType(instance.Type)
	}
	return kind == "" || kind == storage.ArrKindSonarr || kind == storage.ArrKindRadarr
}

func (r *Service) reacquireBrokenFile(
	ctx context.Context,
	broken storage.BrokenFile,
	strategy reacquire.Strategy,
) (*reacquire.Job, error) {
	if r.reacquirer == nil {
		return nil, errReacquirerUnavailable
	}
	entryID, fileID, err := r.resolveBrokenFile(broken)
	var job *reacquire.Job
	if err == nil {
		job, err = r.reacquirer.Reacquire(reacquire.Request{
			EntryID: entryID, FileID: fileID, Cause: reacquire.CauseRepair, Strategy: strategy,
		})
	}
	if errors.Is(err, reacquire.ErrBindingNotFound) || errors.Is(err, errUnmappedBrokenFile) {
		job, err = r.reacquirer.ReacquireLibraryFile(ctx, reacquire.LibraryRequest{
			ArrName: broken.ArrName, ArrFileID: broken.ArrFileID, LibraryPath: broken.SourcePath,
			Cause: reacquire.CauseRepair, Strategy: strategy,
		})
	}
	if err != nil {
		return nil, err
	}
	if job == nil || job.ID == "" {
		return nil, errors.New("arr reacquirer returned no job")
	}
	return job, nil
}

func (r *Service) resolveBrokenFile(broken storage.BrokenFile) (string, string, error) {
	if r.storage == nil {
		return "", "", errors.New("repair storage is unavailable")
	}
	if broken.InfoHash == "" {
		return "", "", fmt.Errorf("%w: no entry ID", errUnmappedBrokenFile)
	}
	if broken.FileName == "" {
		return "", "", fmt.Errorf("%w: no file name", errUnmappedBrokenFile)
	}
	entry, err := r.storage.Get(broken.InfoHash)
	if errors.Is(err, appendstore.ErrKeyNotFound) {
		return "", "", fmt.Errorf("%w: load entry %q: %w", errUnmappedBrokenFile, broken.InfoHash, err)
	}
	if err != nil {
		return "", "", fmt.Errorf("load broken entry: %w", err)
	}
	if entry == nil || entry.InfoHash != broken.InfoHash {
		return "", "", fmt.Errorf("%w: entry %q identity mismatch", reacquire.ErrBindingUnsafe, broken.InfoHash)
	}
	file, ok := entry.Files[broken.FileName]
	if !ok || file == nil {
		return "", "", fmt.Errorf(
			"%w: file %q is not in entry %q",
			errUnmappedBrokenFile,
			broken.FileName,
			broken.InfoHash,
		)
	}
	if file.Deleted {
		return "", "", fmt.Errorf(
			"%w: file %q in entry %q is deleted",
			reacquire.ErrBindingUnsafe,
			broken.FileName,
			broken.InfoHash,
		)
	}
	if file.InfoHash != "" && file.InfoHash != entry.InfoHash {
		return "", "", fmt.Errorf(
			"%w: file %q belongs to entry %q, not %q",
			reacquire.ErrBindingUnsafe,
			broken.FileName,
			file.InfoHash,
			entry.InfoHash,
		)
	}
	if file.ID == "" {
		return "", "", fmt.Errorf(
			"%w: file %q in entry %q has no stable ID",
			errUnmappedBrokenFile,
			broken.FileName,
			broken.InfoHash,
		)
	}
	return entry.InfoHash, file.ID, nil
}
