package arr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"time"
)

type ImportResponseSchema struct {
	Path         string `json:"path"`
	RelativePath string `json:"relativePath"`
	FolderName   string `json:"folderName"`
	Name         string `json:"name"`
	Size         int    `json:"size"`
	Series       struct {
		ID int `json:"id"`
	} `json:"series"`
	Movie struct {
		ID int `json:"id"`
	} `json:"movie"`
	SeasonNumber int `json:"seasonNumber"`
	Episodes     []struct {
		ID int `json:"id"`
	} `json:"episodes"`
	ReleaseGroup string          `json:"releaseGroup"`
	Quality      json.RawMessage `json:"quality"`
	Languages    []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"languages"`
	CustomFormats     []any  `json:"customFormats"`
	CustomFormatScore int    `json:"customFormatScore"`
	IndexerFlags      int    `json:"indexerFlags"`
	ReleaseType       string `json:"releaseType"`
	Rejections        []struct {
		Reason string `json:"reason"`
		Type   string `json:"type"`
	} `json:"rejections"`
	ID    int       `json:"id"`
	Added time.Time `json:"added,omitzero"`
}

type ManualImportFile struct {
	DownloadID   string          `json:"downloadId"`
	FolderName   string          `json:"folderName"`
	Path         string          `json:"path"`
	MovieID      int             `json:"movieId,omitzero"`
	SeriesID     int             `json:"seriesId,omitzero"`
	SeasonNumber int             `json:"seasonNumber,omitzero"`
	EpisodeIDs   []int           `json:"episodeIds,omitempty"`
	Quality      json.RawMessage `json:"quality"`
	Languages    []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"languages"`
	ReleaseGroup      string `json:"releaseGroup"`
	CustomFormats     []any  `json:"customFormats"`
	CustomFormatScore int    `json:"customFormatScore"`
	IndexerFlags      int    `json:"indexerFlags"`
	ReleaseType       string `json:"releaseType,omitempty"`
}

// errAllCandidatesRejected means the arr refused every file of a download, so retrying soon cannot help.
var errAllCandidatesRejected = errors.New("the arr rejected every file")

type importOutcome int

const (
	outcomeNone importOutcome = iota
	outcomeNoFiles
	outcomeRejected
	outcomeWrongEpisode
)

var errNoImportFiles = errors.New("the arr found no files for the download")

var wrongEpisodeRejection = regexp.MustCompile(
	`(?i)^episodes? .+ (was|were) unexpected considering the .+ folder name$`)

func isWrongEpisode(candidate ImportResponseSchema) bool {
	for _, rejection := range candidate.Rejections {
		if wrongEpisodeRejection.MatchString(rejection.Reason) {
			return true
		}
	}
	return false
}

func (s *Service) lookupImport(
	ctx context.Context, instance Arr, downloadID string,
) ([]ImportResponseSchema, importOutcome, error) {
	var candidates []ImportResponseSchema
	query := url.Values{"downloadId": {downloadID}}
	resp, err := s.get(ctx, instance, "api/v3/manualimport?"+query.Encode(), &candidates)
	if err != nil {
		return nil, outcomeNone, fmt.Errorf("manual import lookup: %w", err)
	}
	if expectStatusErr := expectStatus(resp, http.StatusOK); expectStatusErr != nil {
		return nil, outcomeNone, fmt.Errorf("manual import lookup: %w", expectStatusErr)
	}
	if candidates == nil {
		return nil, outcomeNone, errors.New("manual import lookup: no candidate list")
	}
	if len(candidates) == 0 {
		return nil, outcomeNoFiles, fmt.Errorf("manual import: no files found for download %q: %w",
			downloadID, errNoImportFiles)
	}
	if slices.ContainsFunc(candidates, func(c ImportResponseSchema) bool { return len(c.Rejections) == 0 }) {
		return candidates, outcomeNone, nil
	}
	outcome := outcomeRejected
	if slices.IndexFunc(candidates, func(c ImportResponseSchema) bool { return !isWrongEpisode(c) }) < 0 {
		outcome = outcomeWrongEpisode
	}
	return candidates, outcome, fmt.Errorf(
		"manual import: all %d candidates rejected for download %q (%s): %w",
		len(candidates), downloadID, candidates[0].Rejections[0].Reason, errAllCandidatesRejected,
	)
}

// manualImport asks the Arr to import a finished download it did not pick up and returns the queued command.
func (s *Service) manualImport(ctx context.Context, name, downloadID string) (Command, importOutcome, error) {
	instance, err := s.instance(name)
	if err != nil {
		return Command{}, outcomeNone, err
	}

	candidates, outcome, err := s.lookupImport(ctx, instance, downloadID)
	if err != nil {
		return Command{}, outcome, err
	}

	files := make([]ManualImportFile, 0, len(candidates))
	for _, candidate := range candidates {
		if len(candidate.Rejections) > 0 {
			continue
		}
		if instance.Type == Radarr && candidate.Movie.ID <= 0 {
			return Command{}, outcomeNone, fmt.Errorf("manual import: no movie matched for %q", candidate.Path)
		}
		if len(candidate.Quality) == 0 || string(candidate.Quality) == "null" {
			return Command{}, outcomeNone, fmt.Errorf("manual import: no quality returned for %q", candidate.Path)
		}
		episodeIDs := make([]int, 0, len(candidate.Episodes))
		for _, episode := range candidate.Episodes {
			episodeIDs = append(episodeIDs, episode.ID)
		}
		files = append(files, ManualImportFile{
			DownloadID:        downloadID,
			Path:              candidate.Path,
			FolderName:        candidate.FolderName,
			MovieID:           candidate.Movie.ID,
			SeriesID:          candidate.Series.ID,
			SeasonNumber:      candidate.SeasonNumber,
			EpisodeIDs:        episodeIDs,
			Quality:           candidate.Quality,
			Languages:         candidate.Languages,
			ReleaseGroup:      candidate.ReleaseGroup,
			CustomFormats:     candidate.CustomFormats,
			CustomFormatScore: candidate.CustomFormatScore,
			IndexerFlags:      candidate.IndexerFlags,
			ReleaseType:       candidate.ReleaseType,
		})
	}

	command, err := s.command(ctx, instance, struct {
		Name       string             `json:"name"`
		Files      []ManualImportFile `json:"files"`
		ImportMode string             `json:"importMode"`
	}{Name: "ManualImport", Files: files, ImportMode: "copy"})
	if err != nil {
		return Command{}, outcomeNone, fmt.Errorf("manual import: %w", err)
	}
	return command, outcomeNone, nil
}
