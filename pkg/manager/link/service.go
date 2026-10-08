package link

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/flight"
	"github.com/sirrobot01/decypharr/internal/utils"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const (
	MaxReinsertionAttempt = 3
	// maxValidatedEntries caps the validated-link memo map (see GetLink).
	maxValidatedEntries = 8192
)

// EntryRefresher is a function that refreshes an entry by infohash.
type EntryRefresher func(infohash string) (*storage.Entry, error)
type EntryRepairer func(ctx context.Context, entry *storage.Entry) error
type EntrySaver func(entry *storage.Entry) error

// Service handles download link fetching and validation.
// It uses the account-level cache for storing links and only tracks validation state.
type Service struct {
	naming         func() config.WebDavFolderNaming
	validated      *xsync.Map[string, error]
	flights        flight.Group[types.DownloadLink]
	clients        *xsync.Map[string, debrid.Client]
	entryRefresher EntryRefresher
	repairer       EntryRepairer
	entrySaver     EntrySaver
	httpClient     *http.Client
	retries        int
	logger         zerolog.Logger
}

// New creates a new LinkService.
func New(
	clients *xsync.Map[string, debrid.Client],
	entryRefresher EntryRefresher,
	entryReinsert EntryRepairer,
	entrySaver EntrySaver,
	httpClient *http.Client,
	retries int,
	naming func() config.WebDavFolderNaming,
	logger zerolog.Logger,
) *Service {
	return &Service{
		naming:         naming,
		validated:      xsync.NewMap[string, error](),
		clients:        clients,
		entryRefresher: entryRefresher,
		repairer:       entryReinsert,
		entrySaver:     entrySaver,
		httpClient:     httpClient,
		retries:        retries,
		logger:         logger,
	}
}

// folder names entry in messages the way the mount does.
func (s *Service) folder(entry *storage.Entry) string {
	var naming config.WebDavFolderNaming
	if s.naming != nil {
		naming = s.naming()
	}
	return entry.GetFolder(naming)
}

// GetLink fetches and validates a download link for a file in an entry.
// Links are cached at the account level; this service only tracks validation state.
func (s *Service) GetLink(ctx context.Context, entry *storage.Entry, filename string) (types.DownloadLink, error) {
	// Deduplicate concurrent requests for the same file. One caller giving
	// up does not cancel the fetch for the others.
	key := entry.InfoHash + ":" + filename
	dl, _, err := s.flights.Do(ctx, key, func(ctx context.Context) (types.DownloadLink, error) {
		return s.fetchAndValidate(ctx, entry, filename, 0)
	})
	return dl, err
}

// Refresh invalidates a link that failed mid-stream and fetches a replacement.
// It shares GetLink's singleflight key, so a concurrent GetLink may win the
// race and hand back the stale link once more; callers operate on bounded
// retry budgets, so the follow-up attempt lands after the refresh completes.
func (s *Service) Refresh(
	ctx context.Context,
	entry *storage.Entry,
	bad types.DownloadLink,
) (types.DownloadLink, error) {
	if bad.Filename == "" {
		return types.DownloadLink{}, NewPermanentError(ErrEmptyLink, "empty_link")
	}
	key := entry.InfoHash + ":" + bad.Filename
	dl, _, err := s.flights.Do(ctx, key, func(ctx context.Context) (types.DownloadLink, error) {
		return s.invalidateAndRefetch(ctx, entry, bad, 0)
	})
	return dl, err
}

func (s *Service) getClient(provider string) (debrid.Client, error) {
	c, ok := s.clients.Load(provider)
	if !ok {
		return nil, fmt.Errorf("client for provider %s not found", provider)
	}
	return c, nil
}

// fetchAndValidate fetches a download link and validates it.
// attempt tracks how many re-insertion cycles we've already paid for during
// this GetLink call so we can bail out instead of looping forever when the
// underlying file never resolves (see fetchLink/handleBadLink).
func (s *Service) fetchAndValidate(
	ctx context.Context,
	entry *storage.Entry,
	filename string,
	attempt int,
) (types.DownloadLink, error) {
	if err := ctx.Err(); err != nil {
		return types.DownloadLink{}, err
	}
	link, err := s.fetchLink(ctx, entry, filename, attempt)
	if err != nil {
		return s.handleBadLink(ctx, err, entry, link, attempt)
	}

	// Is link already validated
	// Check if we've already validated this link
	if validationErr, exists := s.validated.Load(link.DownloadLink); exists {
		if validationErr == nil {
			return link, nil // Already validated successfully
		}
		// Previous validation failed - check if we should retry
		if linkErr := GetLinkError(validationErr); linkErr != nil && linkErr.ShouldRefetch() {
			return s.invalidateAndRefetch(ctx, entry, link, attempt)
		}
		return types.DownloadLink{}, validationErr
	}

	// Validate the link
	validationErr := s.validateLink(ctx, &link)
	if linkErr := GetLinkError(validationErr); linkErr != nil {
		if retried, handled, retryErr := s.retryInvalidLink(ctx, entry, filename, link, linkErr, attempt); handled {
			return retried, retryErr
		}
	}

	// Store validation result
	// Keys are full download URLs and links rotate on refresh/expiry, so cap
	// the map; resetting merely costs a re-validation per link.
	if s.validated.Size() > maxValidatedEntries {
		s.validated.Clear()
	}
	s.validated.Store(link.DownloadLink, validationErr)

	if validationErr == nil {
		return link, nil
	}
	return types.DownloadLink{}, validationErr
}

// retryInvalidLink reacts to a failed validation: swap accounts, or fetch a
// fresh link. handled is false when the failure should just be recorded.
func (s *Service) retryInvalidLink(
	ctx context.Context,
	entry *storage.Entry,
	filename string,
	link types.DownloadLink,
	linkErr *Error,
	attempt int,
) (types.DownloadLink, bool, error) {
	switch {
	case linkErr.ShouldDisableAccount():
		if err := s.disableLinkAccount(link, linkErr); err != nil {
			s.logger.Error().
				Err(err).
				Str("debrid", link.Debrid).
				Str("token", utils.Mask(link.Token)).
				Str("reason", linkErr.Code).
				Msg("Failed to disable account after link error")
			return types.DownloadLink{}, false, nil
		}
		// The next account serves a new link that needs validating again.
		// Account swap doesn't consume a re-insertion attempt.
		dl, err := s.fetchAndValidate(ctx, entry, filename, attempt)
		return dl, true, err
	case linkErr.ShouldRefetch() || linkErr.ShouldRetry():
		dl, err := s.invalidateAndRefetch(ctx, entry, link, attempt)
		return dl, true, err
	default:
		return types.DownloadLink{}, false, nil
	}
}

func (s *Service) handleBadLink(
	ctx context.Context,
	err error,
	entry *storage.Entry,
	dl types.DownloadLink,
	attempt int,
) (types.DownloadLink, error) {
	if errors.Is(err, customerror.ErrHosterUnavailable) {
		if entry.Bad {
			return types.DownloadLink{}, fmt.Errorf("can't repair %s since it's been marked as bad", s.folder(entry))
		}
		if attempt >= MaxReinsertionAttempt {
			s.markEntryBad(entry, dl.Filename, attempt, "hoster_unavailable")
			return types.DownloadLink{}, fmt.Errorf(
				"entry %s file %s still unresolvable after %d re-insertion attempts",
				s.folder(entry),
				dl.Filename,
				attempt,
			)
		}
		if repairerErr := s.repairer(ctx, entry); repairerErr != nil {
			return types.DownloadLink{}, repairerErr
		}

		if entry.Bad {
			// Entry is still bad
			return types.DownloadLink{}, fmt.Errorf(
				"entry %s(%s) still bad after repair, un-repairable",
				s.folder(entry),
				dl.Link,
			)
		}
		// Bypass singleflight re-entry to avoid deadlock
		return s.fetchAndValidate(ctx, entry, dl.Filename, attempt+1)
	}
	// Just return the error
	return dl, err
}

// markEntryBad sets entry.Bad and persists it so subsequent GetLink calls
// for the same entry short-circuit instead of triggering another re-insertion
// cycle. Logged once per call.
func (s *Service) markEntryBad(entry *storage.Entry, filename string, attempt int, reason string) {
	entry.Bad = true
	if s.entrySaver != nil {
		if err := s.entrySaver(entry); err != nil {
			s.logger.Warn().
				Err(err).
				Str("infohash", entry.InfoHash).
				Msg("Failed to persist Bad flag after exhausting re-insertion attempts")
		}
	}
	s.logger.Warn().
		Str("infohash", entry.InfoHash).
		Str("name", entry.Name).
		Str("filename", filename).
		Int("attempts", attempt).
		Str("reason", reason).
		Msg("Giving up on entry after repeated failed re-insertions")
}

// fetchLink fetches a download link from the debrid provider (via account cache).
func (s *Service) fetchLink(
	ctx context.Context,
	entry *storage.Entry,
	filename string,
	attempt int,
) (types.DownloadLink, error) {
	file, err := entry.GetFile(filename)
	if err != nil {
		return types.DownloadLink{}, NewPermanentError(
			fmt.Errorf("file %s not found in entry %s: %w", filename, entry.Name, err),
			"file_not_found",
		)
	}

	// A refresh hands back a newer entry. It is used for this fetch only:
	// the caller's entry is shared with concurrent readers and stays as is.
	current, placementFile, err := s.getPlacementFile(entry, filename)
	if err != nil {
		return types.DownloadLink{}, err
	}

	if placementFile.Link == "" && placementFile.ID == "" {
		return types.DownloadLink{}, NewPermanentError(
			fmt.Errorf("file link is missing for %s in entry %s", filename, entry.Name),
			"link_missing",
		)
	}

	client, err := s.getClient(current.ActiveProvider)
	if err != nil {
		return types.DownloadLink{}, NewPermanentError(
			fmt.Errorf("debrid client not found: %s", current.ActiveProvider),
			"client_not_found",
		)
	}

	placement := current.Providers[current.ActiveProvider]
	if placement == nil {
		return types.DownloadLink{}, NewPermanentError(
			fmt.Errorf("no placement found for debrid %s with infohash %s", current.ActiveProvider, current.InfoHash),
			"placement_not_found",
		)
	}

	debridFile := &types.File{
		ID:        placementFile.ID,
		Link:      placementFile.Link,
		Path:      placementFile.Path,
		Name:      file.Name,
		Size:      file.Size,
		ByteRange: file.ByteRange,
		Deleted:   file.Deleted,
	}

	// This uses account-level caching internally
	downloadLink, err := client.GetDownloadLink(ctx, placement.ID, debridFile)
	if err != nil {
		return downloadLink, err
	}

	if downloadLink.Empty() {
		// Let's try to reinsert the entry
		if entry.Bad {
			return types.DownloadLink{}, fmt.Errorf("can't repair %s since it's been marked as bad", s.folder(entry))
		}
		if attempt >= MaxReinsertionAttempt {
			s.markEntryBad(entry, filename, attempt, "empty_link")
			return types.DownloadLink{}, fmt.Errorf(
				"entry %s file %s still resolves to an empty link after %d re-insertion attempts",
				s.folder(entry),
				filename,
				attempt,
			)
		}
		if repairerErr := s.repairer(ctx, entry); repairerErr != nil {
			return types.DownloadLink{}, repairerErr
		}

		if entry.Bad {
			// Entry is still bad
			return types.DownloadLink{}, fmt.Errorf(
				"entry %s(%s) still bad after repair, un-repairable",
				s.folder(entry),
				downloadLink.Link,
			)
		}
		// Bypass singleflight re-entry to avoid deadlock
		return s.fetchAndValidate(ctx, entry, filename, attempt+1)
	}

	return downloadLink, nil
}

// getPlacementFile retrieves the placement file with refresh fallback. It
// returns the entry the file belongs to: entry itself, or the refreshed copy.
func (s *Service) getPlacementFile(
	entry *storage.Entry,
	filename string,
) (*storage.Entry, *storage.ProviderFile, error) {
	_, ok := entry.Files[filename]
	if !ok {
		return nil, nil, NewPermanentError(
			fmt.Errorf("file %s not found in entry", filename),
			"file_not_found",
		)
	}

	placement := entry.Providers[entry.ActiveProvider]
	if placement == nil {
		return nil, nil, NewPermanentError(
			fmt.Errorf("no placement found for debrid %s with infohash %s", entry.ActiveProvider, entry.InfoHash),
			"placement_not_found",
		)
	}

	if placementFile := placement.Files[filename]; hasLocator(placementFile) {
		return entry, placementFile, nil
	}
	return s.refreshPlacementFile(entry, filename)
}

func hasLocator(file *storage.ProviderFile) bool {
	return file != nil && (file.Link != "" || file.ID != "")
}

// refreshPlacementFile re-reads the entry from its provider and returns the
// refreshed entry when it now carries a locator for filename. entry is never
// written: callers share it with other readers.
func (s *Service) refreshPlacementFile(
	entry *storage.Entry,
	filename string,
) (*storage.Entry, *storage.ProviderFile, error) {
	if s.entryRefresher == nil {
		return nil, nil, NewPermanentError(
			fmt.Errorf("file %s not available and no refresher configured", filename),
			"no_refresher",
		)
	}

	refreshed, err := s.entryRefresher(entry.InfoHash)
	if err != nil {
		return nil, nil, NewRefetchableError(
			fmt.Errorf("failed to refresh entry: %w", err),
			"refresh_failed",
		)
	}

	if refreshed.Files[filename] == nil {
		return nil, nil, NewPermanentError(
			fmt.Errorf("file disappeared after refresh"),
			"file_disappeared",
		)
	}

	placement := refreshed.Providers[entry.ActiveProvider]
	if placement == nil {
		return nil, nil, NewPermanentError(
			fmt.Errorf("placement disappeared after refresh for debrid %s", entry.ActiveProvider),
			"placement_disappeared",
		)
	}

	placementFile := placement.Files[filename]
	if !hasLocator(placementFile) {
		return nil, nil, NewPermanentError(
			fmt.Errorf("file %s not available after refresh", filename),
			"file_not_available",
		)
	}

	return refreshed, placementFile, nil
}

// validateLink validates a download link by making a HEAD request.
func (s *Service) validateLink(ctx context.Context, link *types.DownloadLink) error {
	if link == nil {
		return NewPermanentError(ErrEmptyLink, "empty_link")
	}
	if link.Empty() {
		return NewPermanentError(fmt.Errorf("download url is empty for %s||%s", link.Filename, link.Link), "empty_link")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, link.DownloadLink, nil)
	if err != nil {
		return NewPermanentError(
			fmt.Errorf("failed to create HEAD request: %w", err),
			"request_creation_failed",
		)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return NewRetryableError(
			fmt.Errorf("HEAD request failed: %w", err),
			"network_error",
		)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}

	errorCode := resp.Header.Get("X-Error")
	if errorCode == "" {
		errorCode = strconv.Itoa(resp.StatusCode)
	}

	return ErrorCodeToLinkError(errorCode)
}

// disableLinkAccount handles errors that require disabling an account.
func (s *Service) disableLinkAccount(link types.DownloadLink, linkErr *Error) error {
	client, err := s.getClient(link.Debrid)
	if err != nil {
		return fmt.Errorf("failed to get client for debrid %s: %w", link.Debrid, err)
	}

	accountManager := client.AccountManager()
	account, err := accountManager.GetAccount(link.Token)
	if err != nil {
		return fmt.Errorf("failed to get account for token %s: %w", utils.Mask(link.Token), err)
	}

	if account == nil {
		return fmt.Errorf("account not found for token %s", utils.Mask(link.Token))
	}

	accountManager.Disable(account)

	// Remove all validations for all the links
	s.validated.Clear()
	s.logger.Warn().
		Str("debrid", link.Debrid).
		Str("token", utils.Mask(account.Token)).
		Str("account", utils.Mask(account.Username())).
		Str("reason", linkErr.Code).
		Msg("Disabled account due to error")
	return nil
}

// invalidateAndRefetch removes a link from both validation tracking and account cache.
func (s *Service) invalidateAndRefetch(
	ctx context.Context,
	entry *storage.Entry,
	link types.DownloadLink,
	attempt int,
) (types.DownloadLink, error) {
	// Remove from validation tracking
	s.validated.Delete(link.DownloadLink)

	// Remove from account cache
	if link.Debrid == "" {
		return types.DownloadLink{}, fmt.Errorf("invalid link")
	}

	client, err := s.getClient(link.Debrid)
	if err != nil {
		return types.DownloadLink{}, err
	}

	_ = client.DeleteLink(link) // This might fail, doesnt matter

	return s.fetchLink(ctx, entry, link.Filename, attempt)
}

// Clear removes all validation tracking entries.
func (s *Service) Clear() {
	s.validated.Clear()
}
