package arr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
		return strings.Contains(text, "not imported or missing from the release"), true
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
	var blocklist, blocklistResearch, blocklistSearchMissing []QueueSchema
	importStatuses := make(map[string]string)
	for _, item := range items {
		switch resolveQueueAction(item, rules) {
		case QueueActionBlocklist:
			blocklist = append(blocklist, item)
		case QueueActionBlocklistResearch:
			blocklistResearch = append(blocklistResearch, item)
		case QueueActionImport:
			if _, seen := importStatuses[item.DownloadID]; !seen {
				importStatuses[item.DownloadID] = statusText(item)
			}
		case QueueActionNone:
			// Nothing to do.
		}
	}

	var searched []QueueSchema
	if len(blocklistResearch) > 0 && s.ResolveType(ctx, name) == Sonarr {
		var arrDecides []QueueSchema
		blocklistResearch, blocklistSearchMissing = splitMappedDownloads(blocklistResearch)
		blocklistSearchMissing, searched, arrDecides = s.planEpisodeSearch(ctx, name, blocklistSearchMissing)
		blocklistResearch = append(blocklistResearch, arrDecides...)
	}
	s.blocklistQueueItems(ctx, name, blocklistResearch, QueueActionBlocklistResearch, false)
	removed := s.blocklistQueueItems(ctx, name, blocklistSearchMissing, QueueActionBlocklistResearch, true)
	s.searchPendingEpisodes(ctx, name, items, searched, removed)
	s.blocklistQueueItems(ctx, name, blocklist, QueueActionBlocklist, true)
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
func resolveQueueAction(item QueueSchema, rules []config.QueueCleanupRule) QueueAction {
	status := strings.ToLower(item.TrackedDownloadStatus)
	if !strings.EqualFold(item.Status, "failed") && status != "warning" && status != "error" {
		return QueueActionNone
	}
	if state := strings.ToLower(item.TrackedDownloadState); state == "importing" || state == "imported" {
		return QueueActionNone
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
		switch QueueAction(rule.Action) {
		case QueueActionImport, QueueActionBlocklist, QueueActionBlocklistResearch:
			return QueueAction(rule.Action)
		case QueueActionNone:
			fallthrough
		default:
			return QueueActionNone
		}
	}
	return QueueActionNone
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

// blocklistQueueItems removes items from the arr's queue with a blocklist, names each one it removed, and reports whether it did.
func (s *Service) blocklistQueueItems(
	ctx context.Context, name string, items []QueueSchema, action QueueAction, skipRedownload bool,
) bool {
	if len(items) == 0 {
		return false
	}
	ids := make([]int, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	if err := s.removeQueueItems(ctx, name, ids, skipRedownload); err != nil {
		s.logger.Error().Err(err).Str("arr", name).Str("action", string(action)).Msg("Queue cleanup: blocklist failed")
		return false
	}
	for _, item := range items {
		s.logger.Info().Str("arr", name).Str("title", item.Title).Str("download_id", item.DownloadID).
			Str("action", string(action)).Str("status", statusText(item)).Msg("Queue cleanup: blocklisted")
	}
	return true
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
}

// searchPendingEpisodes adds the searched downloads' episodes without a file to the arr's pending searches and
// sends those this tick removed or that have left the queue. A pending record seen in this tick's queue is dropped
// first, since the cleanup rules have just judged it again. What the arr refuses stays pending, retried each minute
// for up to an hour; a search whose outcome is unknown is resent only if the arr's command list does not show it.
// shortcut: a record gone from the queue counts as removed by our blocklist, though a user, the arr or a page shift
// in Queue() may have caused it; pending also keeps EpisodeHasFile from blocklist time. Both risk one extra search;
// check FailedHistory and the episode's file before searching an unconfirmed removal if that shows up live.
func (s *Service) searchPendingEpisodes(ctx context.Context, name string, items, searched []QueueSchema, removed bool) {
	s.mu.Lock()
	pending := s.pendingSearches[name]
	if pending == nil {
		pending = make(map[int]pendingSearch)
		s.pendingSearches[name] = pending
	}
	for _, item := range items {
		delete(pending, item.ID)
	}
	for _, item := range searched {
		if !item.EpisodeHasFile {
			pending[item.ID] = pendingSearch{item: item, since: s.now()}
		}
	}
	due := make([]pendingSearch, 0, len(pending))
	for id, search := range pending {
		if s.now().Sub(search.since) >= searchGiveUpAfter {
			delete(pending, id)
			s.logger.Warn().Str("arr", name).Int("episode_id", search.item.EpisodeID).
				Msg("Queue cleanup: episode search not done after 1h, giving up; needs manual intervention")
			continue
		}
		if (!hasQueueID(items, id) || removed) && !s.now().Before(search.retryAt) {
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
	for _, item := range slices.Concat(confirmed, done) {
		delete(pending, item.ID)
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
			pending[tried.item.ID] = search
		}
	}
	s.mu.Unlock()
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

func (s *Service) removeQueueItems(ctx context.Context, name string, ids []int, skipRedownload bool) error {
	instance, err := s.instance(name)
	if err != nil {
		return err
	}
	query := url.Values{
		"removeFromClient": {"true"},
		"blocklist":        {"true"},
		"skipRedownload":   {strconv.FormatBool(skipRedownload)},
		"changeCategory":   {"false"},
	}
	payload := struct {
		IDs []int `json:"ids"`
	}{IDs: ids}

	resp, err := s.mutate(ctx, instance, http.MethodDelete, "api/v3/queue/bulk?"+query.Encode(), payload, nil)
	if err != nil {
		return fmt.Errorf("remove queue items: %w", err)
	}
	if expectSuccessErr := expectSuccess(resp); expectSuccessErr != nil {
		return fmt.Errorf("remove queue items: %w", expectSuccessErr)
	}
	return nil
}
