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
	return m.submitEntry(ctx, newTorrentQueueEntry(importReq, storage.EntryStatusQueued, m.folderNaming()),
		importReq)
}

// stopped reports a ctx that has ended. A provider call whose ctx has ended was cut by a stop (or its owner's end),
// which says nothing about the torrent: its caller changes no state and records no failure.
func stopped(ctx context.Context) bool { return ctx.Err() != nil }

// ProviderSubmitLimit is the longest one provider's submit and status check may take.
const ProviderSubmitLimit = 10 * time.Minute

// errSubmitOutcomeUnknown is a submit cut by its provider's limit before the provider answered: the magnet may have
// reached it, so the grab looks the submit up instead of trying the next provider.
var errSubmitOutcomeUnknown = errors.New("submit outcome unknown")

var errReleaseRefused = errors.New("every provider refused the release")

// submitBudget is the longest a grab's submit child may run: every provider's limit, and a minute to spare.
func (m *Manager) submitBudget() time.Duration {
	clients := m.FilterDebrid(func(common.Client) bool { return true })
	return time.Duration(len(clients))*ProviderSubmitLimit + time.Minute
}

func newTorrentQueueEntry(
	importReq *ImportRequest,
	status storage.EntryStatus,
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
		SavePath: storage.EntrySavePath(
			filepath.Join(importReq.DownloadFolder, importReq.Arr.Name),
			importReq.Magnet.InfoHash,
		),
		Status:           status,
		State:            storage.EntryStateDownloading,
		Progress:         0,
		Action:           importReq.Action,
		DownloadUncached: importReq.DownloadUncached != nil && *importReq.DownloadUncached,
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
	added := placement.AddedAt
	if entry.AddedOn.After(added) {
		added = entry.AddedOn
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
		DownloadUncached: m.uncachedAllowed(entry),
		Added:            added,
	}
}

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
	torrent.SetActive(debridTorrent.Debrid, true)
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

func requestTorrent(r *ImportRequest) *debridTypes.Torrent {
	magnet := *r.Magnet
	return &debridTypes.Torrent{
		InfoHash: magnet.InfoHash,
		Magnet:   &magnet,
		Name:     magnet.Name,
		Size:     magnet.Size,
		Files:    make(map[string]debridTypes.File),
	}
}

// sendToDebrid submits a magnet to the debrid services on ctx: a grab's submit child runs it on its own ctx, which only
// a stop cuts, so the torrent it leaves at the provider gets a queue entry whatever happens to the request that asked.
func (m *Manager) sendToDebrid(
	ctx context.Context,
	importRequest *ImportRequest,
	useHint bool,
) (*debridTypes.Torrent, error) {
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
		pctx, cancel := context.WithTimeout(ctx, ProviderSubmitLimit)
		torrent, err := m.submitToProvider(pctx, db, requestTorrent(importRequest), importRequest, useHint)
		cut := stopped(pctx)
		cancel()
		if err != nil && cut {
			return nil, fmt.Errorf("%s: %w: %w", db.Config().Name, errSubmitOutcomeUnknown, err)
		}
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
	if !slices.ContainsFunc(errs, func(err error) bool { return !submitVerdict(err) }) {
		return nil, fmt.Errorf("%w: %w", errReleaseRefused, joinedErrors)
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
// the provider, the request hash read before the add and the resolved DownloadUncached choice.
func keptSubmission(
	submitted *debridTypes.Torrent,
	provider, hash string,
	downloadUncached bool,
) *debridTypes.Torrent {
	submitted.Status = debridTypes.TorrentStatusDownloading
	submitted.Debrid = provider
	submitted.InfoHash = hash
	submitted.DownloadUncached = downloadUncached
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
	useHint bool,
) (*debridTypes.Torrent, error) {
	overrideDownloadUncached := db.Config().DownloadUncached
	if importRequest.DownloadUncached != nil {
		overrideDownloadUncached = *importRequest.DownloadUncached
	}
	debridTorrent.DownloadUncached = overrideDownloadUncached
	hash := debridTorrent.InfoHash

	decision := m.hearsay.EvaluateAdd(db.Config().Provider, hash)
	_logger := db.Logger()
	if err := m.applyHint(db, decision, importRequest, hash, useHint, overrideDownloadUncached); err != nil {
		return nil, err
	}
	_logger.Info().
		Str("Provider", db.Config().Name).
		Str("Arr", importRequest.Arr.Name).
		Str("Hash", hash).
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
		return keptSubmission(dbt, db.Config().Name, hash, overrideDownloadUncached), nil
	}
	verdict := submitVerdict(err)
	if !verdict && errors.Is(err, customerror.ErrProviderStatusTimeout) && torrent != nil && torrent.ID != "" {
		// The provider has not finished converting the magnet or linking its
		// files. That is no verdict on the release: queue the torrent as still
		// downloading, so the scheduler keeps checking it, and keep it at the
		// provider. Nothing is reported to hearsay.
		m.hearsay.DiscardAdd(decision)
		_logger.Info().
			Str("id", torrent.ID).
			Err(err).
			Msg("Provider still processing the torrent; queued to keep checking")
		return keptSubmission(torrent, db.Config().Name, hash, overrideDownloadUncached), nil
	}
	if !verdict && err != nil && torrent != nil && torrent.ID != "" && transientStatusError(err) &&
		!isTooManyActiveDownloads(err) {
		m.hearsay.DiscardAdd(decision)
		_logger.Info().
			Str("id", torrent.ID).
			Err(err).
			Msg("Provider status check failed for now; queued to keep checking")
		return keptSubmission(torrent, db.Config().Name, hash, overrideDownloadUncached), nil
	}
	reported := errors.Is(err, customerror.ErrTorrentNotCached)
	if reported {
		m.hearsay.RecordAdd(decision, false)
	}
	if err != nil {
		if torrent != nil && torrent.ID != "" {
			// Delete the torrent if it was not downloaded
			provider, id := db.Config().Name, torrent.ID
			_ = m.discardSubmitted(ctx, provider, id, hash)
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
// A magnet that never converted (its grab fails at grab.ConversionLimit) has no seeders: it is dead too.
func releaseIsDead(torrent *debridTypes.Torrent, err error) bool {
	return errors.Is(err, customerror.ErrTorrentNotFound) || errors.Is(err, customerror.ErrTorrentNotCached) ||
		errors.Is(err, customerror.ErrMagnetNotConverted) ||
		(torrent != nil && torrent.Status == debridTypes.TorrentStatusError)
}

func hearsayNotCached(provider, hash string) error {
	return fmt.Errorf("%s: %s recently proven not cached, skipping submit", provider, hash)
}

func submitVerdict(err error) bool {
	return errors.Is(err, customerror.ErrTorrentNotCached) || errors.Is(err, customerror.ErrTorrentBlocked) ||
		errors.Is(err, customerror.ErrReleaseFailed)
}

func (m *Manager) applyHint(
	db common.Client,
	decision hearsay.AddDecision,
	importRequest *ImportRequest,
	hash string,
	useHint, downloadUncached bool,
) error {
	if downloadUncached || !decision.Reject() {
		return nil
	}
	if useHint {
		m.hearsay.DiscardAdd(decision)
		return hearsayNotCached(db.Config().Name, hash)
	}
	logger := db.Logger()
	logger.Info().
		Str("Provider", db.Config().Name).
		Str("Hash", hash).
		Str("Arr", importRequest.Arr.Name).
		Msg("Hearsay hint bypassed: the provider answers")
	return nil
}
