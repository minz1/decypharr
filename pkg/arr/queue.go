package arr

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

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

// CleanupQueue applies the configured cleanup rules to one instance's queue.
func (s *Service) CleanupQueue(ctx context.Context, name string) error {
	items, err := s.Queue(ctx, name)
	if err != nil {
		return err
	}

	rules := s.config.Get().QueueCleanup.Rules
	var blocklist, blocklistResearch []int
	var manualImports []string
	for _, item := range items {
		switch resolveQueueAction(item, rules) {
		case QueueActionBlocklist:
			blocklist = append(blocklist, item.ID)
		case QueueActionBlocklistResearch:
			blocklistResearch = append(blocklistResearch, item.ID)
		case QueueActionImport:
			manualImports = append(manualImports, item.DownloadID)
		case QueueActionNone:
			// Nothing to do.
		}
	}

	if len(blocklistResearch) > 0 {
		if removeQueueItemsErr := s.removeQueueItems(ctx, name, blocklistResearch, false); removeQueueItemsErr != nil {
			s.logger.Error().
				Err(removeQueueItemsErr).
				Str("arr", name).
				Msg("Queue cleanup: blocklist and research failed")
		}
	}
	if len(blocklist) > 0 {
		if removeQueueItemsErr := s.removeQueueItems(ctx, name, blocklist, true); removeQueueItemsErr != nil {
			s.logger.Error().Err(removeQueueItemsErr).Str("arr", name).Msg("Queue cleanup: blocklist failed")
		}
	}
	for _, downloadID := range manualImports {
		if manualImportErr := s.ManualImport(ctx, name, downloadID); manualImportErr != nil {
			s.logger.Error().Err(manualImportErr).Str("arr", name).Msg("Queue cleanup: manual import failed")
		}
	}
	return nil
}

// resolveQueueAction picks what to do with one queue item. Only failed items
// and those flagged warning or error are considered, and the first matching
// rule wins.
func resolveQueueAction(item QueueSchema, rules []config.QueueCleanupRule) QueueAction {
	status := strings.ToLower(item.TrackedDownloadStatus)
	if !strings.EqualFold(item.Status, "failed") && status != "warning" && status != "error" {
		return QueueActionNone
	}

	var builder strings.Builder
	for _, message := range item.StatusMessages {
		builder.WriteString(message.Title)
		builder.WriteByte(' ')
		builder.WriteString(strings.Join(message.Messages, " "))
		builder.WriteByte(' ')
	}
	text := strings.ToLower(builder.String())

	for _, rule := range rules {
		matched := false
		if rule.ID != "" {
			matched, _ = matchCatalogRule(rule.ID, item, text)
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

// removeQueueItems blocklists and removes queue items. skipRedownload keeps the
// Arr from searching for a replacement.
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
