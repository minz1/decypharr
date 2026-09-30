package arr

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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
	SeasonNumber int `json:"seasonNumber"`
	Episodes     []struct {
		ID int `json:"id"`
	} `json:"episodes"`
	ReleaseGroup string `json:"releaseGroup"`
	Quality      struct {
		Quality struct {
			ID         int    `json:"id"`
			Name       string `json:"name"`
			Source     string `json:"source"`
			Resolution int    `json:"resolution"`
		} `json:"quality"`
		Revision struct {
			Version  int  `json:"version"`
			Real     int  `json:"real"`
			IsRepack bool `json:"isRepack"`
		} `json:"revision"`
	} `json:"quality"`
	Languages []struct {
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
	DownloadID   string `json:"downloadId"`
	FolderName   string `json:"folderName"`
	Path         string `json:"path"`
	SeriesID     int    `json:"seriesId"`
	SeasonNumber int    `json:"seasonNumber"`
	EpisodeIDs   []int  `json:"episodeIds"`
	Quality      struct {
		Quality struct {
			ID         int    `json:"id"`
			Name       string `json:"name"`
			Source     string `json:"source"`
			Resolution int    `json:"resolution"`
		} `json:"quality"`
		Revision struct {
			Version  int  `json:"version"`
			Real     int  `json:"real"`
			IsRepack bool `json:"isRepack"`
		} `json:"revision"`
	} `json:"quality"`
	Languages []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"languages"`
	ReleaseGroup      string `json:"releaseGroup"`
	CustomFormats     []any  `json:"customFormats"`
	CustomFormatScore int    `json:"customFormatScore"`
	IndexerFlags      int    `json:"indexerFlags"`
	ReleaseType       string `json:"releaseType"`
	Rejections        []struct {
		Reason string `json:"reason"`
		Type   string `json:"type"`
	} `json:"rejections"`
}

// ManualImport asks the Arr to import a finished download it did not pick up.
func (s *Service) ManualImport(ctx context.Context, name, downloadID string) error {
	instance, err := s.instance(name)
	if err != nil {
		return err
	}

	var candidates []ImportResponseSchema
	query := url.Values{"downloadId": {downloadID}}
	resp, err := s.get(ctx, instance, "api/v3/manualimport?"+query.Encode(), &candidates)
	if err != nil {
		return fmt.Errorf("manual import lookup: %w", err)
	}
	if expectStatusErr := expectStatus(resp, http.StatusOK); expectStatusErr != nil {
		return fmt.Errorf("manual import lookup: %w", expectStatusErr)
	}

	files := make([]ManualImportFile, 0, len(candidates))
	for _, candidate := range candidates {
		episodeIDs := make([]int, 0, len(candidate.Episodes))
		for _, episode := range candidate.Episodes {
			episodeIDs = append(episodeIDs, episode.ID)
		}
		files = append(files, ManualImportFile{
			DownloadID:        downloadID,
			Path:              candidate.Path,
			FolderName:        candidate.FolderName,
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
			Rejections:        candidate.Rejections,
		})
	}

	_, err = s.command(ctx, instance, struct {
		Name       string             `json:"name"`
		Files      []ManualImportFile `json:"files"`
		ImportMode string             `json:"importMode"`
	}{Name: "ManualImport", Files: files, ImportMode: "copy"})
	if err != nil {
		return fmt.Errorf("manual import: %w", err)
	}
	return nil
}
