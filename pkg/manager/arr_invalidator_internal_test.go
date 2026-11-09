package manager

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const invalidatorFingerprint = "v1:test-instance"

// packFiles are the files of a multi-file entry and the Arr file each was imported as.
func packFiles() map[string]int { return map[string]int{"fa": 11, "fb": 12, "fc": 13} }

// packHarness has one entry whose files the Arr imported; confidence says how each file is bound.
func packHarness(t *testing.T, confidence map[string]reacquire.Confidence) (*pruneHarness, []reacquire.Binding) {
	t.Helper()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.mount2(t)
	h.addEntry(t, "pack", func(e *storage.Entry) {
		e.Files = map[string]*storage.File{}
		for id := range packFiles() {
			e.Files[id+".mkv"] = &storage.File{ID: id, Name: id + ".mkv", Size: 10, InfoHash: "pack"}
		}
	})
	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	service, err := reacquire.NewService(g, reacquire.ServiceOptions{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	if startErr := service.Start(t.Context()); startErr != nil {
		t.Fatal(startErr)
	}
	h.m.arrService = service
	var bindings []reacquire.Binding
	for id, arrFileID := range packFiles() {
		binding := reacquire.Binding{
			ArrName: "sonarr", ArrType: arr.Sonarr, ArrInstanceFingerprint: invalidatorFingerprint,
			EntryID: "pack", EntryFileID: id, DownloadID: "download", ArrFileID: arrFileID,
			LibraryPath: "/library/" + id + ".mkv", SeriesID: 3, SeasonNumber: 1, EpisodeIDs: []int{arrFileID},
			Confidence: confidence[id],
		}
		if upsertErr := service.UpsertBinding(binding); upsertErr != nil {
			t.Fatal(upsertErr)
		}
		bindings = append(bindings, binding)
	}
	return h, bindings
}

// authorizing keeps the bindings a job acts on, as enqueue does.
func authorizing(bindings []reacquire.Binding) []reacquire.Binding {
	return slices.DeleteFunc(slices.Clone(bindings), func(binding reacquire.Binding) bool {
		return !binding.AuthorizesMutation()
	})
}

func allExact() map[string]reacquire.Confidence {
	return map[string]reacquire.Confidence{
		"fa": reacquire.ConfidenceExactPath, "fb": reacquire.ConfidenceExactPath, "fc": reacquire.ConfidenceExactPath,
	}
}

// The Arr files a job deletes are exactly the imported files of the entries it
// invalidates: every file of a pack is bound, so invalidating deletes the entry.
func TestInvalidatingAPackMatchesTheArrFilesTheJobDeletes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h, bindings := packHarness(t, allExact())
		job := reacquire.Job{EntryID: "pack", Bindings: authorizing(bindings)}

		var imported, deleted []int
		for id := range packFiles() {
			if binding, ok := h.m.arrService.Lookup("pack", id); ok && binding.ArrFileID > 0 {
				imported = append(imported, binding.ArrFileID)
			}
		}
		for _, binding := range job.Bindings {
			deleted = append(deleted, binding.ArrFileID)
		}
		slices.Sort(imported)
		slices.Sort(deleted)
		if !slices.Equal(imported, deleted) {
			t.Fatalf("job deletes Arr files %v, the entry's imported files are %v", deleted, imported)
		}

		if err := h.m.InvalidateReacquire(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		drainPlacements(t, h.m)
		if h.has("pack") {
			t.Fatal("the invalidated entry is still stored")
		}
		for id := range packFiles() {
			if _, ok := h.m.arrService.Lookup("pack", id); ok {
				t.Errorf("binding of %s survived the invalidation", id)
			}
		}
	})
}

// A sibling imported with a heuristic binding is not in the job, so its Arr
// file would survive while its data is deleted: the entry stays and an
// operator is asked.
func TestInvalidationRefusesAnEntryWithAnImportedFileTheJobDoesNotDelete(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		confidence := allExact()
		confidence["fc"] = reacquire.ConfidenceHeuristic
		h, bindings := packHarness(t, confidence)
		job := reacquire.Job{EntryID: "pack", Bindings: authorizing(bindings)}
		if len(job.Bindings) != 2 {
			t.Fatalf("job has %d bindings, want the 2 that authorize", len(job.Bindings))
		}

		for name, run := range map[string]func() error{
			"check":      func() error { return h.m.CheckInvalidation(context.Background(), job) },
			"invalidate": func() error { return h.m.InvalidateReacquire(context.Background(), job) },
		} {
			if err := run(); !errors.Is(err, reacquire.ErrOperatorRequired) {
				t.Fatalf("%s = %v, want an operator-required refusal", name, err)
			}
		}
		drainPlacements(t, h.m)
		if !h.has("pack") {
			t.Fatal("the entry was deleted")
		}
		requireDeleted(t, h)
		if _, ok := h.m.arrService.Lookup("pack", "fa"); !ok {
			t.Fatal("a binding was removed")
		}
	})
}
