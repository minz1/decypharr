package arr

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

type DownloadClientConfig struct {
	ID                                        int  `json:"id"`
	EnableCompletedDownloadHandling           bool `json:"enableCompletedDownloadHandling"`
	AutoRedownloadFailed                      bool `json:"autoRedownloadFailed"`
	AutoRedownloadFailedFromInteractiveSearch bool `json:"autoRedownloadFailedFromInteractiveSearch"`
}

// RedownloadsFailed reports whether the arr re-searches by itself when the download of grab fails.
func (c DownloadClientConfig) RedownloadsFailed(grab HistoryRecord) bool {
	if !c.AutoRedownloadFailed {
		return false
	}
	releaseSource, _ := grab.DataValue("releaseSource")
	return !strings.EqualFold(releaseSource, "InteractiveSearch") || c.AutoRedownloadFailedFromInteractiveSearch
}

type Command struct {
	ID          int         `json:"id"`
	Name        string      `json:"name"`
	CommandName string      `json:"commandName"`
	Status      string      `json:"status"`
	Message     string      `json:"message,omitempty"`
	Queued      time.Time   `json:"queued,omitzero"`
	Body        CommandBody `json:"body"`
}

type CommandBody struct {
	Name         string `json:"name"`
	EpisodeIDs   []int  `json:"episodeIds,omitempty"`
	SeriesID     int    `json:"seriesId,omitzero"`
	SeasonNumber int    `json:"seasonNumber,omitzero"`
	MovieIDs     []int  `json:"movieIds,omitempty"`
}

type mediaFile struct {
	ID           int    `json:"id"`
	Path         string `json:"path"`
	Size         int64  `json:"size"`
	SeriesID     int    `json:"seriesId"`
	SeasonNumber int    `json:"seasonNumber"`
	MovieID      int    `json:"movieId"`
}

func (s *Service) DownloadClientConfig(ctx context.Context, name string) (DownloadClientConfig, error) {
	instance, err := s.instance(name)
	if err != nil {
		return DownloadClientConfig{}, err
	}

	var clientConfig DownloadClientConfig
	resp, err := s.get(ctx, instance, "api/v3/config/downloadclient", &clientConfig)
	if err != nil {
		return DownloadClientConfig{}, fmt.Errorf("get download client config: %w", err)
	}
	if expectStatusErr := expectStatus(resp, http.StatusOK); expectStatusErr != nil {
		return DownloadClientConfig{}, fmt.Errorf("get download client config: %w", expectStatusErr)
	}
	return clientConfig, nil
}

// LibraryFile reads one imported file by its Arr file ID.
func (s *Service) LibraryFile(ctx context.Context, name string, fileID int) (LibraryFile, bool, error) {
	instance, err := s.instance(name)
	if err != nil {
		return LibraryFile{}, false, err
	}
	if fileID <= 0 {
		return LibraryFile{}, false, fmt.Errorf("get arr file: invalid file ID %d", fileID)
	}
	resource, err := fileResource(instance.Type)
	if err != nil {
		return LibraryFile{}, false, err
	}

	var file mediaFile
	resp, err := s.get(ctx, instance, fmt.Sprintf("api/v3/%s/%d", resource, fileID), &file)
	if err != nil {
		return LibraryFile{}, false, fmt.Errorf("get %s %d: %w", resource, fileID, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return LibraryFile{}, false, nil
	}
	if expectStatusErr := expectStatus(resp, http.StatusOK); expectStatusErr != nil {
		return LibraryFile{}, false, fmt.Errorf("get %s %d: %w", resource, fileID, expectStatusErr)
	}

	libraryFile := LibraryFile{
		ArrFileID:    file.ID,
		Path:         file.Path,
		Size:         file.Size,
		SeriesID:     file.SeriesID,
		SeasonNumber: file.SeasonNumber,
		MovieID:      file.MovieID,
	}
	if instance.Type == Sonarr {
		episodesByFile, sonarrEpisodeIDsErr := s.sonarrEpisodeIDs(ctx, instance, file.SeriesID)
		if sonarrEpisodeIDsErr != nil {
			return LibraryFile{}, false, sonarrEpisodeIDsErr
		}
		libraryFile.EpisodeIDs = episodesByFile[file.ID]
	}
	return libraryFile, true, nil
}

// DeleteLibraryFile removes one imported file. A file the Arr no longer has is
// treated as deleted.
func (s *Service) DeleteLibraryFile(ctx context.Context, name string, fileID int) error {
	instance, err := s.instance(name)
	if err != nil {
		return err
	}
	if fileID <= 0 {
		return fmt.Errorf("delete arr file: invalid file ID %d", fileID)
	}
	resource, err := fileResource(instance.Type)
	if err != nil {
		return err
	}
	var monitored []int
	if instance.Type == Sonarr {
		if monitored, err = s.monitoredEpisodesOfFile(ctx, instance, fileID); err != nil {
			return err
		}
	}

	resp, err := s.mutate(ctx, instance, http.MethodDelete, fmt.Sprintf("api/v3/%s/%d", resource, fileID), nil, nil)
	if err != nil {
		return fmt.Errorf("delete %s %d: %w", resource, fileID, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if expectSuccessErr := expectSuccess(resp); expectSuccessErr != nil {
		return fmt.Errorf("delete %s %d: %w", resource, fileID, expectSuccessErr)
	}
	if monitorErr := s.monitorEpisodes(ctx, instance, monitored); monitorErr != nil {
		s.logger.Error().Err(monitorErr).Str("arr", name).Ints("episode_ids", monitored).Int("file_id", fileID).
			Msg("Deleted an episode file but could not re-monitor its episodes; monitor them in the Arr")
	}
	return nil
}

// monitoredEpisodesOfFile lists the monitored episodes an episode file covers; Sonarr may unmonitor them on delete.
func (s *Service) monitoredEpisodesOfFile(ctx context.Context, instance Arr, fileID int) ([]int, error) {
	var monitored []int
	endpoint := fmt.Sprintf("api/v3/episode?episodeFileId=%d", fileID)
	resp, err := s.getDecoded(ctx, instance, endpoint, arrayResponseDecoder(func(episode sonarrEpisode) error {
		if episode.Monitored {
			monitored = append(monitored, episode.ID)
		}
		return nil
	}))
	if err != nil {
		return nil, fmt.Errorf("list episodes of file %d: %w", fileID, err)
	}
	if expectStatusErr := expectStatus(resp, http.StatusOK); expectStatusErr != nil {
		return nil, fmt.Errorf("list episodes of file %d: %w", fileID, expectStatusErr)
	}
	return monitored, nil
}

func (s *Service) monitorEpisodes(ctx context.Context, instance Arr, episodeIDs []int) error {
	if len(episodeIDs) == 0 {
		return nil
	}
	payload := struct {
		EpisodeIDs []int `json:"episodeIds"`
		Monitored  bool  `json:"monitored"`
	}{EpisodeIDs: episodeIDs, Monitored: true}
	resp, err := s.mutate(ctx, instance, http.MethodPut, "api/v3/episode/monitor", payload, nil)
	if err != nil {
		return err
	}
	return expectSuccess(resp)
}

func (s *Service) SearchEpisodes(ctx context.Context, name string, episodeIDs []int) (Command, error) {
	instance, err := s.instanceOfType(name, Sonarr)
	if err != nil {
		return Command{}, err
	}
	ids, err := uniqueIDs(episodeIDs)
	if err != nil {
		return Command{}, fmt.Errorf("episode search: %w", err)
	}
	return s.command(ctx, instance, struct {
		Name       string `json:"name"`
		EpisodeIDs []int  `json:"episodeIds"`
	}{Name: "EpisodeSearch", EpisodeIDs: ids})
}

func (s *Service) SearchSeason(ctx context.Context, name string, seriesID, seasonNumber int) (Command, error) {
	instance, err := s.instanceOfType(name, Sonarr)
	if err != nil {
		return Command{}, err
	}
	if seriesID <= 0 || seasonNumber < 0 {
		return Command{}, fmt.Errorf("season search: invalid series %d season %d", seriesID, seasonNumber)
	}
	return s.command(ctx, instance, struct {
		Name         string `json:"name"`
		SeriesID     int    `json:"seriesId"`
		SeasonNumber int    `json:"seasonNumber"`
	}{Name: "SeasonSearch", SeriesID: seriesID, SeasonNumber: seasonNumber})
}

func (s *Service) SearchMovies(ctx context.Context, name string, movieIDs []int) (Command, error) {
	instance, err := s.instanceOfType(name, Radarr)
	if err != nil {
		return Command{}, err
	}
	ids, err := uniqueIDs(movieIDs)
	if err != nil {
		return Command{}, fmt.Errorf("movie search: %w", err)
	}
	return s.command(ctx, instance, struct {
		Name     string `json:"name"`
		MovieIDs []int  `json:"movieIds"`
	}{Name: "MoviesSearch", MovieIDs: ids})
}

// RefreshMonitoredDownloads asks the Arr to re-read its download client.
func (s *Service) RefreshMonitoredDownloads(ctx context.Context, name string) error {
	instance, err := s.instance(name)
	if err != nil {
		return err
	}
	_, err = s.command(ctx, instance, struct {
		Name string `json:"name"`
	}{Name: "RefreshMonitoredDownloads"})
	return err
}

func (s *Service) Commands(ctx context.Context, name string) ([]Command, error) {
	instance, err := s.instance(name)
	if err != nil {
		return nil, err
	}

	var commands []Command
	resp, err := s.get(ctx, instance, "api/v3/command", &commands)
	if err != nil {
		return nil, fmt.Errorf("list arr commands: %w", err)
	}
	if expectStatusErr := expectStatus(resp, http.StatusOK); expectStatusErr != nil {
		return nil, fmt.Errorf("list arr commands: %w", expectStatusErr)
	}
	return commands, nil
}

// CommandStatus reads one arr command; found is false when the arr no longer knows it (e.g. after a restart).
func (s *Service) CommandStatus(ctx context.Context, name string, id int) (Command, bool, error) {
	instance, err := s.instance(name)
	if err != nil {
		return Command{}, false, err
	}
	var command Command
	resp, err := s.get(ctx, instance, "api/v3/command/"+strconv.Itoa(id), &command)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return Command{}, false, nil
	}
	if err != nil {
		return Command{}, false, fmt.Errorf("arr command %d: %w", id, err)
	}
	if expectStatusErr := expectStatus(resp, http.StatusOK); expectStatusErr != nil {
		return Command{}, false, fmt.Errorf("arr command %d: %w", id, expectStatusErr)
	}
	return command, true, nil
}

func (s *Service) command(ctx context.Context, instance Arr, payload any) (Command, error) {
	var command Command
	resp, err := s.mutate(ctx, instance, http.MethodPost, "api/v3/command", payload, &command)
	if err != nil {
		if dispatched(resp, err) {
			return Command{}, UnknownMutationOutcome(fmt.Errorf("submit arr command: %w", err), 0)
		}
		return Command{}, fmt.Errorf("submit arr command: %w", err)
	}
	if expectSuccessErr := expectSuccess(resp); expectSuccessErr != nil {
		return Command{}, fmt.Errorf("submit arr command: %w", expectSuccessErr)
	}
	return command, nil
}

func fileResource(kind Type) (string, error) {
	switch kind {
	case Sonarr:
		return "episodefile", nil
	case Radarr:
		return "moviefile", nil
	case Lidarr, Readarr, Others:
		fallthrough
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupportedType, kind)
	}
}

func uniqueIDs(ids []int) ([]int, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("at least one ID is required")
	}
	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("invalid ID %d", id)
		}
	}
	unique := slices.Clone(ids)
	slices.Sort(unique)
	return slices.Compact(unique), nil
}
