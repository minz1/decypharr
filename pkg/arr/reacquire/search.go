package reacquire

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/sirrobot01/decypharr/pkg/arr"
)

func (handler *arrHandler) searchBindings(
	ctx context.Context,
	instance arr.Arr,
	job *Job,
	bindings []Binding,
	progress JobProgress,
) (Status, error) {
	if err := progress.Update(StatusSearching, nil); err != nil {
		return "", err
	}

	mutation, err := searchMutation(instance, bindings)
	if err != nil {
		return "", err
	}
	if round := searchRound(*job); round > 0 {
		// Earlier rounds' searches are settled first, so none stays an intent.
		for _, earlier := range slices.Clone(job.Mutations) {
			if earlier.Kind != mutation.Kind {
				continue
			}
			if sendErr := handler.sendSearch(ctx, instance, job, progress, earlier); sendErr != nil {
				return "", sendErr
			}
		}
		mutation.Key += ":round-" + strconv.Itoa(round)
	}
	mutation, err = ensureMutationIntent(job, progress, StatusSearching, mutation)
	if err != nil {
		return "", err
	}
	if sendErr := handler.sendSearch(ctx, instance, job, progress, mutation); sendErr != nil {
		return "", sendErr
	}
	return StatusWaitingForGrab, nil
}

// sendSearch dispatches a journaled search unless the Arr already shows it.
func (handler *arrHandler) sendSearch(
	ctx context.Context,
	instance arr.Arr,
	job *Job,
	progress JobProgress,
	mutation Mutation,
) error {
	if mutation.State == MutationConfirmed {
		return nil
	}
	lookup := handler.commandReceipt(ctx, instance)
	done, err := reconcileAttempted(job, progress, StatusSearching, mutation, lookup)
	if err != nil || done {
		return err
	}
	mutation, err = recordMutationAttempt(job, progress, StatusSearching, mutation)
	if err != nil {
		return err
	}
	command, dispatchErr := handler.dispatchSearchCommand(ctx, instance, mutation)
	return settleDispatch(job, progress, StatusSearching, mutation, dispatchErr, command.ID, lookup)
}

// searchRound counts the blocklists journaled after the job's first search:
// each needs a search of its own, 0 if none.
func searchRound(job Job) int {
	searched, round := false, 0
	for _, mutation := range job.Mutations {
		switch mutation.Kind {
		case MutationEpisodeSearch, MutationSeasonSearch, MutationMovieSearch:
			searched = true
		case MutationHistoryFailed:
			if searched {
				round++
			}
		case MutationReleaseGrab:
		}
	}
	return round
}

// commandReceipt finds a search command in the Arr's command list.
func (handler *arrHandler) commandReceipt(ctx context.Context, instance arr.Arr) receiptLookup {
	return func(mutation Mutation) (int, bool, error) {
		command, found, err := handler.reconcileCommandMutation(ctx, instance, mutation)
		return command.ID, found, err
	}
}

func searchMutation(instance arr.Arr, bindings []Binding) (Mutation, error) {
	switch instance.Type {
	case arr.Sonarr:
		episodeIDs, seriesID, seasonNumber := sonarrTargets(bindings)
		if len(episodeIDs) > 0 {
			return Mutation{
				Key:         mutationKey(MutationEpisodeSearch, idListKey(episodeIDs)),
				Kind:        MutationEpisodeSearch,
				CommandName: "EpisodeSearch",
				EpisodeIDs:  episodeIDs,
			}, nil
		}
		return Mutation{
			Key:          mutationKey(MutationSeasonSearch, strconv.Itoa(seriesID), strconv.Itoa(seasonNumber)),
			Kind:         MutationSeasonSearch,
			CommandName:  "SeasonSearch",
			SeriesID:     seriesID,
			SeasonNumber: seasonNumber,
		}, nil
	case arr.Radarr:
		movieIDs := movieTargets(bindings)
		return Mutation{
			Key:         mutationKey(MutationMovieSearch, idListKey(movieIDs)),
			Kind:        MutationMovieSearch,
			CommandName: "MoviesSearch",
			MovieIDs:    movieIDs,
		}, nil
	case arr.Lidarr, arr.Readarr, arr.Prowlarr, arr.Others:
		fallthrough
	default:
		return Mutation{}, fmt.Errorf("search unsupported for arr type %q", instance.Type)
	}
}

func (handler *arrHandler) dispatchSearchCommand(
	ctx context.Context,
	instance arr.Arr,
	mutation Mutation,
) (arr.Command, error) {
	switch mutation.Kind {
	case MutationEpisodeSearch:
		return handler.arrs.SearchEpisodes(ctx, instance.Name, mutation.EpisodeIDs)
	case MutationSeasonSearch:
		return handler.arrs.SearchSeason(ctx, instance.Name, mutation.SeriesID, mutation.SeasonNumber)
	case MutationMovieSearch:
		return handler.arrs.SearchMovies(ctx, instance.Name, mutation.MovieIDs)
	case MutationHistoryFailed, MutationReleaseGrab:
		fallthrough
	default:
		return arr.Command{}, fmt.Errorf("unsupported Arr command mutation %q", mutation.Kind)
	}
}

func (handler *arrHandler) reconcileCommandMutation(
	ctx context.Context,
	instance arr.Arr,
	mutation Mutation,
) (arr.Command, bool, error) {
	commands, err := handler.arrs.Commands(ctx, instance.Name)
	if err != nil {
		return arr.Command{}, false, fmt.Errorf("reconcile Arr command: %w", err)
	}
	command, found := findCommandReceipt(commands, mutation)
	return command, found, nil
}
