package reacquire

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/arr"
)

func TestEmptyStrategyNormalizesToCommandSearch(t *testing.T) {
	t.Parallel()
	if got := Strategy("").normalized(); got != StrategyCommandSearch {
		t.Fatalf("empty strategy normalizes to %q, want %q", got, StrategyCommandSearch)
	}
	for _, strategy := range []Strategy{
		StrategyHistoryFailed, StrategyInteractiveBest, StrategyCommandSearch, StrategyDownloadFailed,
	} {
		if got := strategy.normalized(); got != strategy {
			t.Fatalf("%q normalizes to %q", strategy, got)
		}
	}
}

// A request that names no strategy must never blocklist a release.
func TestRequestWithoutStrategySearchesAndBlocklistsNothing(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	rig := newFakeSonarrRig(t, fake, "", 101)
	if rig.job.Strategy != StrategyCommandSearch {
		t.Fatalf("job strategy = %q, want %q", rig.job.Strategy, StrategyCommandSearch)
	}

	waiting := rig.run(t)
	if waiting.Status != StatusWaitingForGrab {
		t.Fatalf("job = %s: %s", waiting.Status, waiting.LastError)
	}
	if blocklists, searches := fake.blocklistCount(), fake.searchCount(); blocklists != 0 || searches != 1 {
		t.Fatalf("blocklists = %d, searches = %d, want 0 and 1", blocklists, searches)
	}
}

// legacyJob is a job an older binary stored with no strategy.
func legacyJob(id string, mutations ...Mutation) Job {
	now := time.Now().UTC()
	instance := arr.Arr{Name: "sonarr", Host: "http://sonarr.test", Token: "secret", Type: arr.Sonarr}
	return Job{
		ID: id, Status: StatusQueued, Cause: CauseManual, ArrName: "sonarr", ArrType: arr.Sonarr,
		EntryID: "entry", FileID: "file", DownloadID: "download",
		Bindings:  []Binding{fakeSonarrBinding(instance, 101)},
		Mutations: mutations, CreatedAt: now, UpdatedAt: now,
	}
}

func TestLegacyJobWithoutStrategyIsConvertedOnLoad(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	blocklisted := Mutation{
		Key: mutationKey(MutationHistoryFailed, "download"), Kind: MutationHistoryFailed,
		State: MutationIntent, DownloadID: "download", HistoryID: 7, IntentAt: now,
	}
	searched := Mutation{
		Key: mutationKey(MutationEpisodeSearch, "101"), Kind: MutationEpisodeSearch, State: MutationIntent,
		CommandName: "EpisodeSearch", EpisodeIDs: []int{101}, IntentAt: now,
	}
	directory := t.TempDir()
	repository, err := OpenReacquireJobRepository(filepath.Join(directory, jobsDatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range []Job{
		legacyJob("blocklisted", blocklisted),
		legacyJob("searched", searched),
		legacyJob("untouched"),
	} {
		if saveErr := repository.SaveDurable(job); saveErr != nil {
			t.Fatal(saveErr)
		}
	}
	if closeErr := repository.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	var logged bytes.Buffer
	service, err := NewService(ServiceOptions{Directory: directory, Logger: zerolog.New(&logged)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	if startErr := service.Start(t.Context()); startErr != nil {
		t.Fatal(startErr)
	}
	if got := strings.Count(logged.String(), "converted it"); got != 3 {
		t.Fatalf("logged %d conversions, want 3:\n%s", got, logged.String())
	}
	for id, want := range map[string]Strategy{
		"blocklisted": StrategyHistoryFailed,
		"searched":    StrategyCommandSearch,
		"untouched":   StrategyCommandSearch,
	} {
		job, ok := service.Job(id)
		if !ok || job.Strategy != want {
			t.Fatalf("job %s strategy = %q (found %v), want %q", id, job.Strategy, ok, want)
		}
	}
}
