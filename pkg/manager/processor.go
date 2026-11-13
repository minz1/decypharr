package manager

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/debrid/common"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/hearsay"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// AddNewTorrent hands a torrent to its grab, which queues it for a provider. Adding a hash already in hand is a
// success, as in qBittorrent.
func (m *Manager) AddNewTorrent(ctx context.Context, importReq *ImportRequest) error {
	if importReq == nil || importReq.Magnet == nil {
		return fmt.Errorf("magnet is required")
	}
	if importReq.Arr.Name == "" {
		return fmt.Errorf("arr is required")
	}
	return m.submitEntry(ctx, newTorrentQueueEntry(importReq, debridTypes.TorrentStatusQueued, m.folderNaming()),
		importReq)
}

// stopped reports a ctx that has ended. A provider call whose ctx has ended was cut by a stop (or its owner's end),
// which says nothing about the torrent: its caller changes no state and records no failure.
func stopped(ctx context.Context) bool { return ctx.Err() != nil }

func newTorrentQueueEntry(
	importReq *ImportRequest,
	status debridTypes.TorrentStatus,
	naming config.WebDavFolderNaming,
) *storage.Entry {
	now := time.Now()
	torrent := &storage.Entry{
		InfoHash:         importReq.Magnet.InfoHash,
		Name:             importReq.Magnet.Name,
		OriginalFilename: importReq.Magnet.Name,
		Protocol:         config.ProtocolTorrent,
		Size:             importReq.Magnet.Size,
		Bytes:            importReq.Magnet.Size,
		Magnet:           importReq.Magnet.Link,
		Category:         importReq.Arr.Name,
		SavePath:         filepath.Join(importReq.DownloadFolder, importReq.Arr.Name),
		Status:           status,
		State:            storage.EntryStateDownloading,
		Progress:         0,
		Action:           importReq.Action,
		CallbackURL:      importReq.CallBackURL,
		SkipMultiSeason:  importReq.skipMultiSeason(),
		Manual:           importReq.Manual,
		CreatedAt:        now,
		UpdatedAt:        now,
		AddedOn:          now,
		Providers:        make(map[string]*storage.ProviderEntry),
		Files:            make(map[string]*storage.File),
		Tags:             []string{},
	}
	torrent.ContentPath = torrent.DownloadPath(naming)
	return torrent
}

// percentScale converts provider percentages to the 0..1 progress fraction.
const percentScale = 100.0

func isTooManyActiveDownloads(err error) bool {
	return errors.Is(err, customerror.ErrTooManyActiveDownloads)
}

// transientStatusError reports whether a status check failed for a reason
// that may pass on its own (network errors, timeouts, provider 5xx, slot
// limits). An uncached torrent is an outcome, not a transient failure.
func transientStatusError(err error) bool {
	return customerror.IsRetriableError(err) && !errors.Is(err, customerror.ErrTorrentNotCached)
}

// statusProbe is the provider torrent a status check of the entry's active placement asks about.
func (m *Manager) statusProbe(entry *storage.Entry, placement *storage.ProviderEntry) *debridTypes.Torrent {
	magnet, err := utils.GetMagnetInfo(entry.Magnet, m.store.Get().AlwaysRmTrackerUrls)
	if err != nil {
		magnet = utils.ConstructMagnet(entry.InfoHash, entry.Name)
	}
	return &debridTypes.Torrent{
		ID:               placement.ID,
		Debrid:           entry.ActiveProvider,
		InfoHash:         entry.InfoHash,
		Magnet:           magnet,
		Name:             entry.Name,
		OriginalFilename: entry.OriginalFilename,
		Size:             entry.Size,
		Files:            make(map[string]debridTypes.File),
		DownloadUncached: entry.DownloadUncached,
	}
}

// maxProviderPendingTime is how long a placement may wait on a provider that has
// not finished processing its torrent (ErrProviderStatusTimeout), links not
// visible yet for one, before the entry fails. The failure carries no verdict on
// the release. A magnet still converting has the shorter maxConversionWait.
const maxProviderPendingTime = 6 * time.Hour

// maxConversionWait is how long a magnet may stay unconverted (ErrMagnetNotConverted).
// One that never converts has no seeders, and Real-Debrid drops it at about 3h on its
// own; failing it first, as a dead release, lets the Arr search another one sooner.
const maxConversionWait = 2 * time.Hour

// logStatusFailure logs a status check the entry fails on; a provider wait that ran out also says which and how long.
func (m *Manager) logStatusFailure(entry *storage.Entry, placement *storage.ProviderEntry, err error) {
	line := m.logger.Error().Err(err).Str("name", entry.Name)
	if wait := providerWaitOf(err); wait != waitNone {
		line = line.Stringer("wait", wait).Dur("waited", providerWaitAge(entry, placement))
	}
	line.Msg("Error checking status")
}

// providerWaitAge is how long the placement has waited on its provider, measured from the placement, else from the entry.
func providerWaitAge(entry *storage.Entry, placement *storage.ProviderEntry) time.Duration {
	start := cmp.Or(placement.AddedAt, entry.AddedOn)
	if start.IsZero() {
		return 0
	}
	return time.Since(start).Truncate(time.Second)
}

// linksWaitLimit is how long a finished torrent may go without file links before its grab fails.
const linksWaitLimit = 6 * time.Hour

// awaitingLinks reports a downloaded torrent whose files the entry can't take yet: it knows none and some lack a link.
func awaitingLinks(entry *storage.Entry, finished *debridTypes.Torrent) bool {
	return len(entry.Files) == 0 && len(finished.Files) > 0 && !isComplete(finished.Files)
}

// refreshFinishedPlacement stores the finished placement, and the entry's files if it had none, once every file has a link.
func refreshFinishedPlacement(entry *storage.Entry, old *storage.ProviderEntry, finished *debridTypes.Torrent) {
	if finished.Debrid != entry.ActiveProvider || !isComplete(finished.Files) {
		return
	}
	if len(entry.Files) == 0 {
		addEntryFiles(entry, finished)
	}
	placement := entry.AddTorrentProvider(finished)
	placement.AddedAt = old.AddedAt
	placement.Progress = 1
	now := time.Now()
	placement.DownloadedAt = &now
}

func applyDebridTorrentToEntry(torrent *storage.Entry, debridTorrent *debridTypes.Torrent) {
	_ = torrent.AddTorrentProvider(debridTorrent)
	torrent.ActiveProvider = debridTorrent.Debrid
	torrent.Bytes = debridTorrent.GetSize()
	torrent.Size = debridTorrent.GetSize()
	torrent.Name = debridTorrent.Name
	torrent.OriginalFilename = debridTorrent.OriginalFilename
	torrent.UpdatedAt = time.Now()

	addEntryFiles(torrent, debridTorrent)

	if debridTorrent.Status != debridTypes.TorrentStatusDownloaded {
		return
	}
	if placement := torrent.GetActiveProvider(); placement != nil {
		now := time.Now()
		placement.DownloadedAt = &now
		placement.Progress = 1.0
	}
}

func addEntryFiles(entry *storage.Entry, debridTorrent *debridTypes.Torrent) {
	for _, file := range debridTorrent.Files {
		entry.Files[file.Name] = &storage.File{
			Name:      file.Name,
			Size:      file.Size,
			ByteRange: file.ByteRange,
			Deleted:   file.Deleted,
			InfoHash:  entry.InfoHash,
			AddedOn:   entry.AddedOn,
		}
	}
}

// SendToDebrid submits a magnet to debrid service(s) - replaces debrid.Parse. The provider calls run on the manager's
// work ctx, not on a caller's: a submit belongs to the manager, and the torrent it leaves at the provider must get a
// queue entry whatever happens to the request that asked.
func (m *Manager) SendToDebrid(importRequest *ImportRequest) (*debridTypes.Torrent, error) {
	return m.sendToDebrid(m.work.Context(), importRequest)
}

// sendToDebrid is SendToDebrid on ctx: a grab's submit child runs it on its own ctx, which only a stop cuts.
func (m *Manager) sendToDebrid(ctx context.Context, importRequest *ImportRequest) (*debridTypes.Torrent, error) {
	debridTorrent := &debridTypes.Torrent{
		InfoHash: importRequest.Magnet.InfoHash,
		Magnet:   importRequest.Magnet,
		Name:     importRequest.Magnet.Name,
		Size:     importRequest.Magnet.Size,
		Files:    make(map[string]debridTypes.File),
	}

	clients := m.FilterDebrid(func(c common.Client) bool {
		if importRequest.SelectedDebrid != "" && c.Config().Name != importRequest.SelectedDebrid {
			return false
		}
		return true
	})

	if len(clients) == 0 {
		return nil, fmt.Errorf("no debrid clients available")
	}

	errs := make([]error, 0, len(clients))
	var throttled []string

	for _, db := range clients {
		torrent, err := m.submitToProvider(ctx, db, debridTorrent, importRequest)
		if err != nil {
			errs = append(errs, err)
			if isTooManyActiveDownloads(err) {
				throttled = append(throttled, db.Config().Name)
			}
			continue
		}
		return torrent, nil
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("failed to process torrent: no clients available")
	}
	joinedErrors := fmt.Errorf("failed to process torrent: %w", errors.Join(errs...))
	// The release is refused for good only if every provider refused it for good.
	if slices.ContainsFunc(errs, isPermanentRefusal) &&
		slices.ContainsFunc(errs, func(err error) bool { return !isPermanentRefusal(err) }) {
		joinedErrors = customerror.NewError(joinedErrors, http.StatusBadGateway, "provider_error", false, false).
			Retryable()
	}
	if len(throttled) > 0 {
		return nil, &throttledError{providers: throttled, err: joinedErrors}
	}
	return nil, joinedErrors
}

func isPermanentRefusal(err error) bool {
	typed, ok := errors.AsType[*customerror.Error](err)
	return ok && typed.IsPermanent()
}

// throttledError names the providers whose active-download cap refused a submit.
type throttledError struct {
	providers []string
	err       error
}

func (e *throttledError) Error() string { return e.err.Error() }
func (e *throttledError) Unwrap() error { return e.err }

// keptSubmission is the submit result of a stopped add, queued as still downloading. The result need not name its
// provider (AllDebrid sets only the ID), and an entry with no active provider is never rebuilt into a job, so it takes
// the provider, hash and DownloadUncached choice from the request's torrent.
func keptSubmission(submitted *debridTypes.Torrent, provider string, input *debridTypes.Torrent) *debridTypes.Torrent {
	submitted.Status = debridTypes.TorrentStatusDownloading
	submitted.Debrid = provider
	submitted.InfoHash = input.InfoHash
	submitted.DownloadUncached = input.DownloadUncached
	return submitted
}

// reportRefusedSubmit tells hearsay about a submit that returned no torrent: only the provider's block counts as a
// verdict, and not one that came with a stop.
func (m *Manager) reportRefusedSubmit(ctx context.Context, decision hearsay.AddDecision, err error) {
	if errors.Is(err, customerror.ErrTorrentBlocked) && !stopped(ctx) {
		m.hearsay.RecordAdd(decision, false)
		return
	}
	m.hearsay.DiscardAdd(decision)
}

// submitToProvider submits the magnet to one provider and checks it landed,
// recording the cache outcome with hearsay.
func (m *Manager) submitToProvider(
	ctx context.Context,
	db common.Client,
	debridTorrent *debridTypes.Torrent,
	importRequest *ImportRequest,
) (*debridTypes.Torrent, error) {
	overrideDownloadUncached := db.Config().DownloadUncached
	if importRequest.DownloadUncached != nil {
		overrideDownloadUncached = *importRequest.DownloadUncached
	}
	debridTorrent.DownloadUncached = overrideDownloadUncached

	decision := m.hearsay.EvaluateAdd(db.Config().Provider, debridTorrent.InfoHash)
	if !overrideDownloadUncached && decision.Reject() {
		m.hearsay.DiscardAdd(decision)
		return nil, fmt.Errorf(
			"%s: %s recently proven not cached, skipping submit",
			db.Config().Name,
			debridTorrent.InfoHash,
		)
	}
	_logger := db.Logger()
	_logger.Info().
		Str("Provider", db.Config().Name).
		Str("Arr", importRequest.Arr.Name).
		Str("Hash", debridTorrent.InfoHash).
		Str("Name", debridTorrent.Name).
		Str("Action", string(importRequest.Action)).
		Msg("Processing torrent")

	dbt, err := db.SubmitMagnet(ctx, debridTorrent)
	if err != nil || dbt == nil || dbt.ID == "" {
		m.reportRefusedSubmit(ctx, decision, err)
		if err == nil {
			err = fmt.Errorf("%s returned an empty torrent after submission", db.Config().Name)
		}
		return nil, err
	}
	_logger.Info().Str("id", dbt.ID).Msgf("Entry: %s submitted to %s", dbt.Name, db.Config().Name)

	torrent, err := db.CheckStatus(ctx, dbt)
	if stopped(ctx) {
		// A stop is no verdict on a torrent the provider already holds, whatever the check made of it: queue the
		// submit result as still downloading and keep it at the provider.
		m.hearsay.DiscardAdd(decision)
		_logger.Info().Str("id", dbt.ID).Err(err).Msg("Stopped after the submit; queued to keep checking")
		return keptSubmission(dbt, db.Config().Name, debridTorrent), nil
	}
	if errors.Is(err, customerror.ErrProviderStatusTimeout) && torrent != nil && torrent.ID != "" {
		// The provider has not finished converting the magnet or linking its
		// files. That is no verdict on the release: queue the torrent as still
		// downloading, so the scheduler keeps checking it, and keep it at the
		// provider. Nothing is reported to hearsay.
		m.hearsay.DiscardAdd(decision)
		torrent.Status = debridTypes.TorrentStatusDownloading
		_logger.Info().
			Str("id", torrent.ID).
			Err(err).
			Msg("Provider still processing the torrent; queued to keep checking")
		return torrent, nil
	}
	reported := errors.Is(err, customerror.ErrTorrentNotCached)
	if reported {
		m.hearsay.RecordAdd(decision, false)
	}
	if err != nil {
		if torrent != nil && torrent.ID != "" {
			// Delete the torrent if it was not downloaded
			provider, id := db.Config().Name, torrent.ID
			m.startPlacementRun(debridTorrent.Name, provider, func(ctx context.Context) {
				m.discardSubmitted(ctx, provider, id, debridTorrent.InfoHash)
			})
		}
		if !reported {
			m.hearsay.DiscardAdd(decision)
		}
		return nil, err
	}
	if torrent == nil {
		m.hearsay.DiscardAdd(decision)
		return nil, fmt.Errorf("torrent %s returned nil after checking status", dbt.Name)
	}
	m.hearsay.RecordAdd(decision, torrent.Status == debridTypes.TorrentStatusDownloaded)
	return torrent, nil
}

// releaseIsDead reports a status-check failure the provider pins on the torrent itself, not on decypharr or the account.
// A magnet that never converted (it reaches here only once maxConversionWait is spent) has no seeders: it is dead too.
func releaseIsDead(torrent *debridTypes.Torrent, err error) bool {
	return errors.Is(err, customerror.ErrTorrentNotFound) ||
		errors.Is(err, customerror.ErrMagnetNotConverted) ||
		(torrent != nil && torrent.Status == debridTypes.TorrentStatusError)
}
