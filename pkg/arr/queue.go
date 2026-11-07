package arr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

type QueueAction string

const (
	QueueActionNone              QueueAction = ""
	QueueActionImport            QueueAction = "import"
	QueueActionBlocklist         QueueAction = "blacklist"
	QueueActionBlocklistResearch QueueAction = "blacklist_research"
	// QueueActionRemoveLeftover is internal, never a config value: remove the row without a blocklist.
	QueueActionRemoveLeftover QueueAction = "remove_leftover"
)

type QueueResponseScheme struct {
	Page          int           `json:"page"`
	PageSize      int           `json:"pageSize"`
	SortKey       string        `json:"sortKey"`
	SortDirection string        `json:"sortDirection"`
	TotalRecords  int           `json:"totalRecords"`
	Records       []QueueSchema `json:"records"`
}

type QueueSchema struct {
	SeriesID              int    `json:"seriesId"`
	EpisodeID             int    `json:"episodeId"`
	SeasonNumber          int    `json:"seasonNumber"`
	Title                 string `json:"title"`
	Status                string `json:"status"`
	TrackedDownloadStatus string `json:"trackedDownloadStatus"`
	TrackedDownloadState  string `json:"trackedDownloadState"`
	StatusMessages        []struct {
		Title    string   `json:"title"`
		Messages []string `json:"messages"`
	} `json:"statusMessages"`
	DownloadID                          string `json:"downloadId"`
	Protocol                            string `json:"protocol"`
	DownloadClient                      string `json:"downloadClient"`
	DownloadClientHasPostImportCategory bool   `json:"downloadClientHasPostImportCategory"`
	Indexer                             string `json:"indexer"`
	OutputPath                          string `json:"outputPath"`
	EpisodeHasFile                      bool   `json:"episodeHasFile"`
	ID                                  int    `json:"id"`

	// rule and provider are set by queue cleanup on a row it means to blocklist: the cleanup rule that matched and
	// the provider the download is judged under. They are never on the wire.
	rule     string
	provider string
}

// matchCatalogRule applies the built-in cleanup rule id, a
// config.QueueCleanupRule.ID. text is the lowercased join of a queue item's
// status message titles and messages. The second result is false for an
// unknown id.
func matchCatalogRule(id string, item QueueSchema, text string) (bool, bool) {
	switch id {
	case "failed_download":
		return strings.EqualFold(item.Status, "failed"), true
	case "title_mismatch":
		return strings.Contains(text, "title mismatch"), true
	case "matched_by_id":
		return strings.Contains(text, "matched to") && strings.Contains(text, "by id"), true
	case "unable_to_parse":
		return strings.Contains(text, "unable to parse download"), true
	case "no_eligible_files":
		return strings.Contains(text, "no files found are eligible"), true
	case "episodes_missing":
		return strings.Contains(text, "not imported or missing from the release") &&
			!onlyNonUpgradeLeftovers(item), true
	case "file_empty":
		return strings.Contains(text, "file is empty"), true
	case "invalid_local_path":
		return strings.Contains(text, "is not a valid local path"), true
	case "not_grabbed":
		return strings.Contains(text, "not in a category"), true
	default:
		return false, false
	}
}

var nonUpgradeLeftover = regexp.MustCompile(
	`(?i)not (a custom format upgrade|an upgrade|a quality revision upgrade) for existing episode file\(s\)`)

// onlyNonUpgradeLeftovers reports whether item has per-file messages and each says the file is no upgrade.
func onlyNonUpgradeLeftovers(item QueueSchema) bool {
	seen := false
	for _, status := range item.StatusMessages {
		for _, message := range status.Messages {
			if !nonUpgradeLeftover.MatchString(message) {
				return false
			}
			seen = true
		}
	}
	return seen
}

func (s *Service) Queue(ctx context.Context, name string) ([]QueueSchema, error) {
	instance, err := s.instance(name)
	if err != nil {
		return nil, err
	}

	items := make([]QueueSchema, 0)
	for page := 1; ; page++ {
		query := url.Values{"page": {strconv.Itoa(page)}, "pageSize": {"200"}}
		var response QueueResponseScheme
		resp, getErr := s.get(ctx, instance, "api/v3/queue?"+query.Encode(), &response)
		if getErr != nil {
			return items, getErr
		}
		if expectStatusErr := expectStatus(resp, http.StatusOK); expectStatusErr != nil {
			return items, expectStatusErr
		}

		items = append(items, response.Records...)
		if len(response.Records) == 0 || len(items) >= response.TotalRecords {
			return items, nil
		}
	}
}

const (
	maxImportAttempts   = 5
	importRetryDelay    = time.Hour
	importRejectedDelay = 24 * time.Hour
	searchGiveUpAfter   = time.Hour
	searchRetryDelay    = time.Minute
	commandClockSkew    = 2 * time.Second
	episodeReadBatch    = 100 // episode IDs per api/v3/episode request, to keep the URL short
)

type importFailure struct {
	attempts  int
	status    string
	retryAt   time.Time
	commandID int       // the arr's in-flight ManualImport command, 0 when none
	waitSince time.Time // when the in-flight command was submitted or last reported stuck
}

// CleanupQueue applies the configured cleanup rules to one instance's queue.
func (s *Service) CleanupQueue(ctx context.Context, name string) error {
	items, err := s.Queue(ctx, name)
	if err != nil {
		return err
	}

	rules := s.config.Get().QueueCleanup.Rules
	if refreshErr := s.refreshEpisodeFiles(ctx, name, items, rules); refreshErr != nil {
		return refreshErr
	}
	complete := completeDownloads(items)
	var blocklist, blocklistResearch, blocklistSearchMissing, leftovers []QueueSchema
	importStatuses := make(map[string]string)
	for _, item := range items {
		action, rule := resolveQueueRule(item, rules, complete[item.DownloadID])
		switch action {
		case QueueActionRemoveLeftover:
			leftovers = append(leftovers, item)
		case QueueActionBlocklist:
			item.rule, item.provider = rule, s.guard.guardProvider(item)
			blocklist = append(blocklist, item)
		case QueueActionBlocklistResearch:
			item.rule, item.provider = rule, s.guard.guardProvider(item)
			blocklistResearch = append(blocklistResearch, item)
		case QueueActionImport:
			if _, seen := importStatuses[item.DownloadID]; !seen {
				importStatuses[item.DownloadID] = statusText(item)
			}
		case QueueActionNone:
			// Nothing to do.
		}
	}

	// Only so many distinct downloads are blocklisted per window. What is held stays in the queue, before anything is
	// planned for it, so a held download owes no search and is judged again next tick.
	blocklistResearch, blocklist = s.holdExcessBlocklists(ctx, name, blocklistResearch, blocklist)

	var searched []QueueSchema
	if len(blocklistResearch) > 0 && s.ResolveType(ctx, name) == Sonarr {
		var arrDecides []QueueSchema
		blocklistResearch, blocklistSearchMissing = splitMappedDownloads(blocklistResearch)
		blocklistSearchMissing, searched, arrDecides = s.planEpisodeSearch(ctx, name, blocklistSearchMissing)
		blocklistResearch = append(blocklistResearch, arrDecides...)
	}
	s.blocklistQueueItems(ctx, name, blocklistResearch, QueueActionBlocklistResearch, false)
	s.dropRejudgedSearches(name, items)
	// The arr forgets a download removed this way without searching for it, so the search is owed from before the
	// removal is sent. If the owed searches cannot be recorded, nothing is removed.
	if s.planSearches(name, searched) {
		outcome := s.blocklistQueueItems(ctx, name, blocklistSearchMissing, QueueActionBlocklistResearch, true)
		s.settleSearches(name, searched, outcome)
	}
	s.searchPendingEpisodes(ctx, name, items)
	s.blocklistQueueItems(ctx, name, blocklist, QueueActionBlocklist, true)
	s.removeLeftovers(ctx, name, leftovers)
	s.mu.RLock()
	previous := s.importFailures[name]
	s.mu.RUnlock()
	next := make(map[string]importFailure, len(importStatuses))
	for downloadID, status := range importStatuses {
		next[downloadID] = s.retryImport(ctx, name, downloadID, status, previous[downloadID])
	}
	s.mu.Lock()
	s.importFailures[name] = next
	s.mu.Unlock()
	return nil
}

// retryImport advances one stuck import: it waits on an in-flight arr command, else submits a new one unless backed off.
func (s *Service) retryImport(
	ctx context.Context, name, downloadID, status string, failure importFailure,
) importFailure {
	if failure.status != status {
		failure = importFailure{status: status, commandID: failure.commandID}
	}
	if failure.commandID != 0 {
		command, found, err := s.CommandStatus(ctx, name, failure.commandID)
		running := found &&
			(strings.EqualFold(command.Status, "queued") || strings.EqualFold(command.Status, "started"))
		if err != nil || running {
			if s.now().Sub(failure.waitSince) >= importRetryDelay {
				failure.waitSince = s.now()
				s.logger.Warn().Err(err).Str("arr", name).Str("download_id", downloadID).
					Int("command_id", failure.commandID).Str("command_status", command.Status).
					Msg("Queue cleanup: manual import command still unfinished after 1h, needs manual intervention")
			}
			return failure
		}
		finished := fmt.Errorf(
			"manual import command %d no longer known to the arr; item still queued",
			failure.commandID,
		)
		if found {
			finished = fmt.Errorf("manual import command %d %s, item still queued: %s",
				failure.commandID, command.Status, command.Message)
		}
		failure.commandID = 0
		return s.recordImportFailure(name, downloadID, failure, finished)
	}
	if s.now().Before(failure.retryAt) {
		return failure
	}

	command, err := s.ManualImport(ctx, name, downloadID)
	if err == nil && command.ID == 0 {
		err = fmt.Errorf("manual import: arr returned no command id")
	}
	if err != nil {
		return s.recordImportFailure(name, downloadID, failure, err)
	}
	failure.commandID = command.ID
	failure.waitSince = s.now()
	s.logger.Info().Str("arr", name).Str("download_id", downloadID).Int("command_id", command.ID).
		Msg("Queue cleanup: manual import submitted")
	return failure
}

// recordImportFailure counts one failed import and backs it off after maxImportAttempts.
func (s *Service) recordImportFailure(name, downloadID string, failure importFailure, importErr error) importFailure {
	if errors.Is(importErr, errAllCandidatesRejected) {
		failure.retryAt = s.now().Add(importRejectedDelay)
		s.logger.Warn().Err(importErr).Str("arr", name).Str("download_id", downloadID).
			Dur("retry_in", importRejectedDelay).
			Msg("Queue cleanup: the arr rejects every file of this download; needs manual intervention")
		return failure
	}
	failure.attempts++
	if failure.attempts < maxImportAttempts {
		s.logger.Error().Err(importErr).Str("arr", name).Str("download_id", downloadID).
			Int("attempt", failure.attempts).Msg("Queue cleanup: manual import failed")
		return failure
	}
	failure.retryAt = s.now().Add(importRetryDelay)
	s.logger.Warn().Err(importErr).Str("arr", name).Str("download_id", downloadID).Int("attempts", failure.attempts).
		Msg("Queue cleanup: manual import keeps failing; retrying in 1h, needs manual intervention")
	return failure
}

// resolveQueueAction picks what to do with one queue item. Only failed items
// and those flagged warning or error are considered, and the first matching
// rule wins. Catalog rules skip items the Arr's own failed download handling
// already blocklists and re-searches.
func resolveQueueAction(item QueueSchema, rules []config.QueueCleanupRule, complete bool) QueueAction {
	action, _ := resolveQueueRule(item, rules, complete)
	return action
}

// resolveQueueRule is resolveQueueAction that also names the rule it matched: the catalog ID, or the match text of
// a custom rule.
func resolveQueueRule(item QueueSchema, rules []config.QueueCleanupRule, complete bool) (QueueAction, string) {
	status := strings.ToLower(item.TrackedDownloadStatus)
	if !strings.EqualFold(item.Status, "failed") && status != "warning" && status != "error" {
		return QueueActionNone, ""
	}
	if state := strings.ToLower(item.TrackedDownloadState); state == "importing" || state == "imported" {
		return QueueActionNone, ""
	}
	arrFailed := strings.EqualFold(item.TrackedDownloadState, "failedPending") ||
		strings.EqualFold(item.TrackedDownloadState, "failed")

	text := statusText(item)

	for _, rule := range rules {
		matched := false
		if rule.ID != "" {
			matched, _ = matchCatalogRule(rule.ID, item, text)
			matched = matched && !arrFailed
		} else if needle := strings.ToLower(strings.TrimSpace(rule.Match)); needle != "" {
			matched = strings.Contains(text, needle)
		}
		if !matched {
			continue
		}
		ruleKey := rule.ID
		if ruleKey == "" {
			ruleKey = "match:" + strings.ToLower(strings.TrimSpace(rule.Match))
		}
		switch QueueAction(rule.Action) {
		case QueueActionBlocklist, QueueActionBlocklistResearch:
			if complete && rule.ID == "episodes_missing" {
				return QueueActionRemoveLeftover, ruleKey
			}
			return QueueAction(rule.Action), ruleKey
		case QueueActionImport:
			return QueueAction(rule.Action), ruleKey
		case QueueActionNone, QueueActionRemoveLeftover:
			fallthrough
		default:
			return QueueActionNone, ruleKey
		}
	}
	return QueueActionNone, ""
}

// refreshEpisodeFiles sets episodeHasFile from Sonarr's episodes for the records a blocklist rule acts on, then
// rechecks the episodes still owed a search. The queue's own value can't be used: Sonarr freezes it when a download
// completes, so the episodes a pack has just imported read as missing. A failed read of the queue's episodes fails
// the tick, so nothing is decided on the frozen value.
func (s *Service) refreshEpisodeFiles(
	ctx context.Context,
	name string,
	items []QueueSchema,
	rules []config.QueueCleanupRule,
) error {
	var ids []int
	for _, item := range items {
		action := resolveQueueAction(item, rules, false)
		if item.EpisodeID > 0 && (action == QueueActionBlocklist || action == QueueActionBlocklistResearch) {
			ids = append(ids, item.EpisodeID)
		}
	}
	s.mu.RLock()
	owed := len(s.pendingSearches[name]) > 0
	s.mu.RUnlock()
	if len(ids) == 0 && !owed || s.ResolveType(ctx, name) != Sonarr {
		return nil
	}
	if len(ids) > 0 {
		slices.Sort(ids)
		files, err := s.episodeFiles(ctx, name, slices.Compact(ids))
		if err != nil {
			return err
		}
		for i := range items {
			if hasFile, ok := files[items[i].EpisodeID]; ok {
				items[i].EpisodeHasFile = hasFile
			}
		}
	}
	s.recheckPendingSearches(ctx, name)
	return nil
}

// recheckPendingSearches drops a due owed search whose episode has a file now or no longer exists, and holds one it
// can't read for a minute. Each episode is read on its own: Sonarr fails a batch read naming a deleted episode.
func (s *Service) recheckPendingSearches(ctx context.Context, name string) {
	s.mu.RLock()
	records := make(map[int][]int)
	for id, search := range s.pendingSearches[name] {
		if !s.now().Before(search.retryAt) {
			records[search.item.EpisodeID] = append(records[search.item.EpisodeID], id)
		}
	}
	s.mu.RUnlock()
	for episodeID, ids := range records {
		hasFile, exists, err := s.episodeFile(ctx, name, episodeID)
		s.mu.Lock()
		pending := s.pendingSearches[name]
		for _, id := range ids {
			search, ok := pending[id]
			switch {
			case !ok:
			case err != nil:
				if held := s.now().Add(searchRetryDelay); search.retryAt.Before(held) {
					search.retryAt = held
					pending[id] = search
				}
			case hasFile || !exists:
				s.dropSearchLocked(name, id)
			}
		}
		s.mu.Unlock()
		switch {
		case err != nil:
			s.logger.Debug().Err(err).Str("arr", name).Int("episode_id", episodeID).
				Msg("Queue cleanup: owed episode search held, the episode could not be read")
		case hasFile || !exists:
			s.logger.Debug().Str("arr", name).Int("episode_id", episodeID).Bool("deleted", !exists).
				Msg("Queue cleanup: owed episode search dropped, the episode has a file or was deleted")
		}
	}
}

// episodeFile reads one episode: whether it has a file, and whether it still exists.
func (s *Service) episodeFile(ctx context.Context, name string, episodeID int) (bool, bool, error) {
	instance, err := s.instance(name)
	if err != nil {
		return false, false, err
	}
	var episode sonarrEpisode
	resp, err := s.get(ctx, instance, fmt.Sprintf("api/v3/episode/%d", episodeID), &episode)
	if err != nil {
		return false, false, fmt.Errorf("read episode %d: %w", episodeID, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return false, false, nil
	}
	if expectStatusErr := expectStatus(resp, http.StatusOK); expectStatusErr != nil {
		return false, false, fmt.Errorf("read episode %d: %w", episodeID, expectStatusErr)
	}
	return episode.EpisodeFileID > 0, true, nil
}

// episodeFiles reports, per episode ID, whether the episode has a file now.
func (s *Service) episodeFiles(ctx context.Context, name string, episodeIDs []int) (map[int]bool, error) {
	instance, err := s.instance(name)
	if err != nil {
		return nil, err
	}
	files := make(map[int]bool, len(episodeIDs))
	for chunk := range slices.Chunk(episodeIDs, episodeReadBatch) {
		query := url.Values{}
		for _, id := range chunk {
			query.Add("episodeIds", strconv.Itoa(id))
		}
		resp, getErr := s.getDecoded(ctx, instance, "api/v3/episode?"+query.Encode(),
			arrayResponseDecoder(func(episode sonarrEpisode) error {
				files[episode.ID] = episode.EpisodeFileID > 0
				return nil
			}))
		if getErr != nil {
			return nil, fmt.Errorf("read episode files: %w", getErr)
		}
		if expectStatusErr := expectStatus(resp, http.StatusOK); expectStatusErr != nil {
			return nil, fmt.Errorf("read episode files: %w", expectStatusErr)
		}
	}
	return files, nil
}

// completeDownloads holds the download IDs whose every queue record has its episode on disk.
func completeDownloads(items []QueueSchema) map[string]bool {
	complete := make(map[string]bool)
	for _, item := range items {
		if item.DownloadID == "" {
			continue
		}
		if has, seen := complete[item.DownloadID]; !seen || has {
			complete[item.DownloadID] = item.EpisodeHasFile
		}
	}
	return complete
}

// statusText is the lowercased join of a queue item's status message titles and messages.
func statusText(item QueueSchema) string {
	var builder strings.Builder
	for _, message := range item.StatusMessages {
		builder.WriteString(message.Title)
		builder.WriteByte(' ')
		builder.WriteString(strings.Join(message.Messages, " "))
		builder.WriteByte(' ')
	}
	return strings.ToLower(builder.String())
}

// splitMappedDownloads separates the downloads with a record lacking an episode from those whose every record has one.
// The arr's re-search covers every episode of a download, so only a fully mapped one can be searched episode by episode.
func splitMappedDownloads(items []QueueSchema) ([]QueueSchema, []QueueSchema) {
	var unmapped, mapped []QueueSchema
	hasUnmapped := make(map[string]bool)
	for _, item := range items {
		if item.EpisodeID == 0 {
			hasUnmapped[item.DownloadID] = true
		}
	}
	for _, item := range items {
		if hasUnmapped[item.DownloadID] {
			unmapped = append(unmapped, item)
		} else {
			mapped = append(mapped, item)
		}
	}
	return unmapped, mapped
}

// removalOutcome is how a bulk removal ended: the arr took it, refused it, or may have taken it.
type removalOutcome int

const (
	removalNone    removalOutcome = iota // nothing was sent
	removalDone                          // the arr answered with success
	removalRefused                       // the arr answered with a refusal, or the request never left
	removalUnknown                       // the request may have reached the arr without a clear answer
)

// blocklistQueueItems removes items from the arr's queue with a blocklist, names each one it removed, and reports how it ended.
func (s *Service) blocklistQueueItems(
	ctx context.Context, name string, items []QueueSchema, action QueueAction, skipRedownload bool,
) removalOutcome {
	if len(items) == 0 {
		return removalNone
	}
	ids := make([]int, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	if err := s.removeQueueItems(ctx, name, ids, true, skipRedownload); err != nil {
		s.logger.Error().Err(err).Str("arr", name).Str("action", string(action)).Msg("Queue cleanup: blocklist failed")
		if errors.Is(err, ErrMutationOutcomeUnknown) {
			// It may have been sent, so it counts toward the window.
			s.guard.commit(name, items, s.now())
			return removalUnknown
		}
		return removalRefused
	}
	s.guard.commit(name, items, s.now())
	for _, item := range items {
		s.logger.Info().Str("arr", name).Str("title", item.Title).Str("download_id", item.DownloadID).
			Str("action", string(action)).Str("status", statusText(item)).Msg("Queue cleanup: blocklisted")
	}
	return removalDone
}

// removeLeftovers removes the rows of packs that did their job, keeping the release and the client's copy.
func (s *Service) removeLeftovers(ctx context.Context, name string, items []QueueSchema) {
	if len(items) == 0 {
		return
	}
	ids := make([]int, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	if err := s.removeQueueItems(ctx, name, ids, false, false); err != nil {
		s.logger.Error().Err(err).Str("arr", name).Msg("Queue cleanup: leftover removal failed")
		return
	}
	for _, item := range items {
		s.logger.Info().Str("arr", name).Str("title", item.Title).Str("download_id", item.DownloadID).
			Str("status", statusText(item)).Msg("Queue cleanup: removed leftover row, every episode has a file")
	}
}

// planEpisodeSearch splits fully mapped downloads by who searches for their missing episodes, as Sonarr's
// failed-download settings would: blocklisted holds every download decypharr blocklists without the arr's
// re-search, searched the ones of those it searches itself, and arrDecides the ones whose settings or grab
// history it could not read, which keep the arr's own re-search.
func (s *Service) planEpisodeSearch(
	ctx context.Context, name string, items []QueueSchema,
) ([]QueueSchema, []QueueSchema, []QueueSchema) {
	if len(items) == 0 {
		return nil, nil, nil
	}
	settings, err := s.DownloadClientConfig(ctx, name)
	if err != nil {
		s.logger.Warn().Err(err).Str("arr", name).
			Msg("Queue cleanup: download client settings unreadable, the arr re-searches")
		return nil, nil, items
	}
	if !settings.AutoRedownloadFailed {
		return items, nil, nil
	}
	var blocklisted, searched, arrDecides []QueueSchema
	redownloads := make(map[string]bool)
	unreadable := make(map[string]bool)
	for _, item := range items {
		downloadID := item.DownloadID
		if _, seen := redownloads[downloadID]; seen || unreadable[downloadID] {
			continue
		}
		if settings.AutoRedownloadFailedFromInteractiveSearch {
			redownloads[downloadID] = true
			continue
		}
		grab, _, grabErr := s.GrabHistory(ctx, name, downloadID)
		if grabErr != nil {
			s.logger.Warn().Err(grabErr).Str("arr", name).Str("download_id", downloadID).
				Msg("Queue cleanup: grab history unreadable, the arr re-searches")
			unreadable[downloadID] = true
			continue
		}
		redownloads[downloadID] = settings.RedownloadsFailed(grab)
	}
	for _, item := range items {
		if unreadable[item.DownloadID] {
			arrDecides = append(arrDecides, item)
			continue
		}
		blocklisted = append(blocklisted, item)
		if redownloads[item.DownloadID] {
			searched = append(searched, item)
		}
	}
	return blocklisted, searched, arrDecides
}

// pendingSearch is a blocklisted queue record whose episode search is still owed, when it was first owed, when a
// search for it was last sent with an unknown outcome, and when it may next be tried.
type pendingSearch struct {
	item    QueueSchema
	since   time.Time
	sentAt  time.Time
	retryAt time.Time
	state   ledgerState
	// restored marks a search that was owed before this process started. It is not given up on until it was tried once.
	restored bool
}

// dropRejudgedSearches drops a planned owed search whose record is in this tick's queue: the cleanup rules have just
// judged it again, and either it was never removed or they will plan it anew. An armed one stays, since the arr
// accepted its removal.
func (s *Service) dropRejudgedSearches(name string, items []QueueSchema) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range items {
		if search, ok := s.pendingSearches[name][item.ID]; ok && search.state != ledgerArmed {
			s.dropSearchLocked(name, item.ID)
		}
	}
}

// dropSearchLocked forgets an owed search, in memory and in the ledger. Caller must hold s.mu.
func (s *Service) dropSearchLocked(name string, queueID int) {
	delete(s.pendingSearches[name], queueID)
	if err := s.ledger.remove(name, queueID); err != nil {
		s.logger.Warn().Err(err).Str("arr", name).Int("queue_id", queueID).
			Msg("Queue cleanup: could not drop an owed search from the ledger; it may be searched again after a restart")
	}
}

// planSearches records the searches owed for items about to be removed without the arr's re-search, in memory and on
// disk, before the removal is sent. It reports false, having planned nothing, when the ledger cannot take them.
func (s *Service) planSearches(name string, searched []QueueSchema) bool {
	var planned []pendingSearch
	s.mu.RLock()
	for _, item := range searched {
		if _, owed := s.pendingSearches[name][item.ID]; !item.EpisodeHasFile && !owed {
			planned = append(planned, pendingSearch{item: item, since: s.now(), state: ledgerPlanned})
		}
	}
	s.mu.RUnlock()
	rows := make([]ledgerRow, len(planned))
	for i, search := range planned {
		rows[i] = ledgerRow{Arr: name, State: ledgerPlanned, Item: search.item, Since: search.since}
	}
	if err := s.currentLedger().put(rows...); err != nil {
		s.logger.Error().Err(err).Str("arr", name).
			Msg("Queue cleanup: the owed searches could not be recorded, so nothing is removed this tick")
		return false
	}
	if len(planned) == 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pendingSearches[name]
	if pending == nil {
		pending = make(map[int]pendingSearch)
		s.pendingSearches[name] = pending
	}
	for _, search := range planned {
		pending[search.item.ID] = search
	}
	return true
}

// settleSearches moves the searches planned for a removal on from planned: armed when the arr took the removal,
// abandoned when it refused it. A removal of unknown outcome leaves them planned, to be searched once their record
// has left the queue.
func (s *Service) settleSearches(name string, searched []QueueSchema, outcome removalOutcome) {
	if outcome != removalDone && outcome != removalRefused {
		return
	}
	var rows []ledgerRow
	s.mu.Lock()
	for _, item := range searched {
		search, ok := s.pendingSearches[name][item.ID]
		if !ok || search.state != ledgerPlanned {
			continue
		}
		if outcome == removalRefused {
			delete(s.pendingSearches[name], item.ID)
			rows = append(rows, ledgerRow{Arr: name, State: ledgerAbandoned, Item: item, Since: search.since})
			continue
		}
		search.state = ledgerArmed
		s.pendingSearches[name][item.ID] = search
		rows = append(rows, ledgerRow{Arr: name, State: ledgerArmed, Item: item, Since: search.since})
	}
	s.mu.Unlock()
	if err := s.currentLedger().put(rows...); err != nil {
		s.logger.Warn().Err(err).Str("arr", name).
			Msg("Queue cleanup: the ledger could not be updated; a restart searches these once their record has left the queue")
	}
}

// searchPendingEpisodes sends the owed searches that are due: those whose removal the arr took (armed), and those
// whose record has left the queue, which covers a removal of unknown outcome and a restart after one. What the arr
// refuses stays pending, retried each minute for up to an hour, except that a search owed from before a restart is
// tried once whatever its age; a search whose outcome is unknown is resent only if the arr's command list does not
// show it.
// shortcut: a planned record gone from the queue counts as removed by our blocklist, though a user, the arr or a page
// shift in Queue() may have caused it. That risks one extra search, for an episode still without a file
// (refreshEpisodeFiles drops the rest); check FailedHistory before searching an unconfirmed removal if it shows up live.
func (s *Service) searchPendingEpisodes(ctx context.Context, name string, items []QueueSchema) {
	s.mu.Lock()
	pending := s.pendingSearches[name]
	due := make([]pendingSearch, 0, len(pending))
	for id, search := range pending {
		if !search.restored && s.now().Sub(search.since) >= searchGiveUpAfter {
			s.dropSearchLocked(name, id)
			s.logger.Warn().Str("arr", name).Int("episode_id", search.item.EpisodeID).
				Msg("Queue cleanup: episode search not done after 1h, giving up; needs manual intervention")
			continue
		}
		if (search.state == ledgerArmed || !hasQueueID(items, id)) && !s.now().Before(search.retryAt) {
			due = append(due, search)
		}
	}
	s.mu.Unlock()
	if len(due) == 0 {
		return
	}

	items, confirmed := s.unsentSearches(ctx, name, due)
	slices.SortFunc(items, func(a, b QueueSchema) int { return a.EpisodeID - b.EpisodeID })
	sentAt := s.now()
	done, unknown := s.searchEpisodes(ctx, name, items)
	s.mu.Lock()
	defer s.mu.Unlock()
	pending = s.pendingSearches[name]
	for _, item := range slices.Concat(confirmed, done) {
		s.dropSearchLocked(name, item.ID)
	}
	for _, item := range unknown {
		if search, ok := pending[item.ID]; ok {
			search.sentAt = sentAt
			pending[item.ID] = search
		}
	}
	for _, tried := range due {
		if search, ok := pending[tried.item.ID]; ok {
			search.retryAt = sentAt.Add(searchRetryDelay)
			search.restored = false
			pending[tried.item.ID] = search
		}
	}
}

// unsentSearches returns the due items to search now and those the arr's command list shows were already received.
// An item whose earlier search had an unknown outcome waits while the command list can't be read.
// shortcut: a held item can be resent if the arr drops the finished command meanwhile; persist command ids if seen.
func (s *Service) unsentSearches(ctx context.Context, name string, due []pendingSearch) ([]QueueSchema, []QueueSchema) {
	var send, confirmed []QueueSchema
	var commands []Command
	var commandsErr error
	listed := false
	for _, search := range due {
		if search.sentAt.IsZero() {
			send = append(send, search.item)
			continue
		}
		if !listed {
			commands, commandsErr = s.Commands(ctx, name)
			listed = true
			if commandsErr != nil {
				s.logger.Warn().Err(commandsErr).Str("arr", name).
					Msg("Queue cleanup: arr commands unreadable, holding searches of unknown outcome")
			}
		}
		switch {
		case commandsErr != nil:
		case slices.ContainsFunc(commands, func(command Command) bool { return searchReceived(command, search) }):
			confirmed = append(confirmed, search.item)
		default:
			send = append(send, search.item)
		}
	}
	return send, confirmed
}

// searchReceived reports whether command is a search the arr queued for search's episode since it was sent.
func searchReceived(command Command, search pendingSearch) bool {
	if command.Queued.Before(search.sentAt.Add(-commandClockSkew)) {
		return false
	}
	switch {
	case strings.EqualFold(command.Body.Name, "EpisodeSearch"):
		return slices.Contains(command.Body.EpisodeIDs, search.item.EpisodeID)
	case strings.EqualFold(command.Body.Name, "SeasonSearch"):
		return command.Body.SeriesID == search.item.SeriesID && command.Body.SeasonNumber == search.item.SeasonNumber
	default:
		return false
	}
}

func hasQueueID(items []QueueSchema, id int) bool {
	return slices.ContainsFunc(items, func(item QueueSchema) bool { return item.ID == id })
}

// searchEpisodes sends one SeasonSearch per season whose every episode is in items, as Sonarr's own re-search
// does, and one EpisodeSearch for the rest. It returns the items whose search the arr accepted, and those whose
// search it may have received.
func (s *Service) searchEpisodes(ctx context.Context, name string, items []QueueSchema) ([]QueueSchema, []QueueSchema) {
	type season struct{ series, number int }
	bySeason := make(map[season][]QueueSchema)
	var seasons []season
	for _, item := range items {
		key := season{item.SeriesID, item.SeasonNumber}
		if _, seen := bySeason[key]; !seen {
			seasons = append(seasons, key)
		}
		bySeason[key] = append(bySeason[key], item)
	}

	var done, unknown, episodes []QueueSchema
	for _, key := range seasons {
		group := bySeason[key]
		if !s.coversSeason(ctx, name, key.series, key.number, group) {
			episodes = append(episodes, group...)
			continue
		}
		if _, err := s.SearchSeason(ctx, name, key.series, key.number); err != nil {
			if errors.Is(err, ErrMutationOutcomeUnknown) {
				unknown = append(unknown, group...)
			}
			s.logger.Error().Err(err).Str("arr", name).Int("series_id", key.series).Int("season", key.number).
				Msg("Queue cleanup: season search failed")
			continue
		}
		s.logger.Info().Str("arr", name).Int("series_id", key.series).Int("season", key.number).
			Msg("Queue cleanup: searched a season without any file")
		done = append(done, group...)
	}
	if len(episodes) == 0 {
		return done, unknown
	}

	episodeIDs := make([]int, len(episodes))
	for i, item := range episodes {
		episodeIDs[i] = item.EpisodeID
	}
	if _, err := s.SearchEpisodes(ctx, name, episodeIDs); err != nil {
		if errors.Is(err, ErrMutationOutcomeUnknown) {
			unknown = append(unknown, episodes...)
		}
		s.logger.Error().Err(err).Str("arr", name).Ints("episode_ids", episodeIDs).
			Msg("Queue cleanup: episode search failed")
		return done, unknown
	}
	s.logger.Info().Str("arr", name).Ints("episode_ids", episodeIDs).
		Msg("Queue cleanup: searched episodes without a file")
	return append(done, episodes...), unknown
}

// coversSeason reports whether group holds more than one episode and every episode the arr lists for the season.
func (s *Service) coversSeason(ctx context.Context, name string, seriesID, seasonNumber int, group []QueueSchema) bool {
	if len(group) < 2 || seriesID <= 0 {
		return false
	}
	seasonIDs, err := s.seasonEpisodeIDs(ctx, name, seriesID, seasonNumber)
	if err != nil {
		s.logger.Warn().Err(err).Str("arr", name).Int("series_id", seriesID).Int("season", seasonNumber).
			Msg("Queue cleanup: season episodes unreadable, searching episode by episode")
		return false
	}
	return len(seasonIDs) > 0 && !slices.ContainsFunc(seasonIDs, func(id int) bool {
		return !slices.ContainsFunc(group, func(item QueueSchema) bool { return item.EpisodeID == id })
	})
}

// removeQueueItems removes rows from the arr's queue; a blocklisting removal also drops the client's copy.
func (s *Service) removeQueueItems(ctx context.Context, name string, ids []int, blocklist, skipRedownload bool) error {
	instance, err := s.instance(name)
	if err != nil {
		return err
	}
	query := url.Values{
		"removeFromClient": {strconv.FormatBool(blocklist)},
		"blocklist":        {strconv.FormatBool(blocklist)},
		"skipRedownload":   {strconv.FormatBool(skipRedownload)},
		"changeCategory":   {"false"},
	}
	payload := struct {
		IDs []int `json:"ids"`
	}{IDs: ids}

	resp, err := s.mutate(ctx, instance, http.MethodDelete, "api/v3/queue/bulk?"+query.Encode(), payload, nil)
	if err != nil {
		if dispatched(resp, err) {
			return UnknownMutationOutcome(fmt.Errorf("remove queue items: %w", err), 0)
		}
		return fmt.Errorf("remove queue items: %w", err)
	}
	if expectSuccessErr := expectSuccess(resp); expectSuccessErr != nil {
		expectSuccessErr = fmt.Errorf("remove queue items: %w", expectSuccessErr)
		if resp.StatusCode >= http.StatusInternalServerError {
			return UnknownMutationOutcome(expectSuccessErr, 0)
		}
		return expectSuccessErr
	}
	return nil
}
