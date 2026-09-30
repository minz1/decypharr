package arr

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"

	"golang.org/x/sync/errgroup"
)

// contentBatchSize bounds one bulk delete or search, which Arr applies in a
// single database transaction.
const contentBatchSize = 50

type Content struct {
	Title string        `json:"title"`
	ID    int           `json:"id"`
	Files []ContentFile `json:"files"`
}

type ContentFile struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	ID           int    `json:"id"`
	EpisodeID    int    `json:"showId"`
	FileID       int    `json:"fileId"`
	TargetPath   string `json:"targetPath"`
	EntryName    string `json:"entryName,omitempty"`
	IsSymlink    bool   `json:"isSymlink"`
	IsBroken     bool   `json:"isBroken"`
	SeasonNumber int    `json:"seasonNumber"`
	Processed    bool   `json:"processed"`
	Size         int64  `json:"size"`
}

// Delete removes the file from disk. Sonarr's bulk delete regularly fails to,
// which leaves a broken symlink behind.
func (f *ContentFile) Delete() {
	_ = os.Remove(f.Path)
}

type Movie struct {
	Title         string `json:"title"`
	OriginalTitle string `json:"originalTitle"`
	Path          string `json:"path"`
	MovieFile     struct {
		MovieID      int    `json:"movieId"`
		RelativePath string `json:"relativePath"`
		Path         string `json:"path"`
		ID           int    `json:"id"`
		Size         int64  `json:"size"`
	} `json:"movieFile"`
	ID int `json:"id"`
}

// Media enumerates the library as repair sees it: one Content per series or
// movie, with the files it has imported. An empty mediaID returns everything.
func (s *Service) Media(ctx context.Context, name, mediaID string) ([]Content, error) {
	instance, err := s.instance(name)
	if err != nil {
		return nil, err
	}
	if instance.Type == Radarr {
		return s.movies(ctx, instance, mediaID)
	}

	var series []struct {
		Title string `json:"title"`
		ID    int    `json:"id"`
	}
	resp, err := s.get(ctx, instance, "api/v3/series?"+url.Values{"tvdbId": {mediaID}}.Encode(), &series)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return s.movies(ctx, instance, mediaID)
	}
	if expectStatusErr := expectStatus(resp, http.StatusOK); expectStatusErr != nil {
		return nil, fmt.Errorf("list series: %w", expectStatusErr)
	}

	contents := make([]Content, 0, len(series))
	for _, item := range series {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return contents, ctxErr
		}
		files, sonarrSeriesFilesErr := s.sonarrSeriesFiles(ctx, instance, item.ID)
		if sonarrSeriesFilesErr != nil {
			continue
		}
		content := Content{Title: item.Title, ID: item.ID, Files: make([]ContentFile, 0, len(files))}
		for _, file := range files {
			episodeID := 0
			if len(file.EpisodeIDs) > 0 {
				episodeID = file.EpisodeIDs[0]
			}
			content.Files = append(content.Files, ContentFile{
				FileID:       file.ArrFileID,
				Path:         file.Path,
				ID:           item.ID,
				EpisodeID:    episodeID,
				SeasonNumber: file.SeasonNumber,
				Size:         file.Size,
			})
		}
		if len(content.Files) == 0 {
			continue
		}
		contents = append(contents, content)
	}
	return contents, nil
}

func (s *Service) movies(ctx context.Context, instance Arr, mediaID string) ([]Content, error) {
	var movies []Movie
	resp, err := s.get(ctx, instance, "api/v3/movie?"+url.Values{"tmdbId": {mediaID}}.Encode(), &movies)
	if err != nil {
		return nil, err
	}
	if expectStatusErr := expectStatus(resp, http.StatusOK); expectStatusErr != nil {
		return nil, fmt.Errorf("list movies: %w", expectStatusErr)
	}

	contents := make([]Content, 0, len(movies))
	for _, movie := range movies {
		if movie.MovieFile.ID == 0 || movie.MovieFile.Path == "" {
			continue
		}
		contents = append(contents, Content{
			Title: movie.Title,
			ID:    movie.ID,
			Files: []ContentFile{{
				FileID: movie.MovieFile.ID,
				ID:     movie.ID,
				Path:   movie.MovieFile.Path,
				Size:   movie.MovieFile.Size,
			}},
		})
	}
	return contents, nil
}

// SearchMissing asks the Arr to look for replacements for the given files.
func (s *Service) SearchMissing(ctx context.Context, name string, files []ContentFile) error {
	instance, err := s.instance(name)
	if err != nil || len(files) == 0 {
		return err
	}
	for batch := range slices.Chunk(files, contentBatchSize) {
		switch instance.Type {
		case Sonarr:
			err = s.searchSonarrSeasons(ctx, instance, batch)
		case Radarr:
			err = s.searchRadarrMovies(ctx, instance, batch)
		default:
			return fmt.Errorf("%w: %s", ErrUnsupportedType, instance.Type)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) searchSonarrSeasons(ctx context.Context, instance Arr, files []ContentFile) error {
	seasons := make(map[[2]int]struct{}, len(files))
	for _, file := range files {
		seasons[[2]int{file.ID, file.SeasonNumber}] = struct{}{}
	}

	group, groupCtx := errgroup.WithContext(ctx)
	// Each command writes to Sonarr's command table, and its single SQLite
	// writer turns parallel posts into "database is locked" failures.
	group.SetLimit(1)
	for season := range seasons {
		group.Go(func() error {
			_, err := s.command(groupCtx, instance, struct {
				Name         string `json:"name"`
				SeriesID     int    `json:"seriesId"`
				SeasonNumber int    `json:"seasonNumber"`
			}{Name: "SeasonSearch", SeriesID: season[0], SeasonNumber: season[1]})
			return err
		})
	}
	return group.Wait()
}

func (s *Service) searchRadarrMovies(ctx context.Context, instance Arr, files []ContentFile) error {
	ids := make([]int, 0, len(files))
	for _, file := range files {
		ids = append(ids, file.ID)
	}
	_, err := s.command(ctx, instance, struct {
		Name     string `json:"name"`
		MovieIDs []int  `json:"movieIds"`
	}{Name: "MoviesSearch", MovieIDs: ids})
	return err
}

// DeleteFiles removes imported files in bulk, then removes whatever the Arr
// left on disk.
func (s *Service) DeleteFiles(ctx context.Context, name string, files []ContentFile) error {
	instance, err := s.instance(name)
	if err != nil || len(files) == 0 {
		return err
	}
	resource, err := fileResource(instance.Type)
	if err != nil {
		return err
	}
	field := map[Type]string{Sonarr: "episodeFileIds", Radarr: "movieFileIds"}[instance.Type]

	for batch := range slices.Chunk(files, contentBatchSize) {
		ids := make([]int, 0, len(batch))
		for _, file := range batch {
			// Sonarr rejects the whole batch when an ID repeats.
			if file.FileID != 0 && !slices.Contains(ids, file.FileID) {
				ids = append(ids, file.FileID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		resp, mutateErr := s.mutate(
			ctx,
			instance,
			http.MethodDelete,
			"api/v3/"+resource+"/bulk",
			map[string][]int{field: ids},
			nil,
		)
		if mutateErr != nil {
			return fmt.Errorf("delete %s bulk: %w", resource, mutateErr)
		}
		if expectSuccessErr := expectSuccess(resp); expectSuccessErr != nil {
			return fmt.Errorf("delete %s bulk: %w", resource, expectSuccessErr)
		}
		for i := range batch {
			batch[i].Delete()
		}
	}
	return nil
}
