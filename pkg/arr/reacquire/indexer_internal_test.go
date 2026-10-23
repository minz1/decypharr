package reacquire

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
)

func TestMatchLibraryFilesBySymlinkTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "managed", "Movie.Release", "movie.mkv")
	libraryPath := filepath.Join(dir, "library", "Movie.mkv")
	if err := os.MkdirAll(filepath.Dir(libraryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, libraryPath); err != nil {
		t.Fatal(err)
	}

	matches, _ := matchLibraryFiles(
		[]arr.LibraryFile{{ArrFileID: 42, Path: libraryPath}},
		[]ManagedFile{{EntryID: "entry", EntryFolder: "Movie.Release", FileID: "file", FileName: "movie.mkv"}},
		filepath.Join(dir, "managed"),
	)
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}
	if matches[0].managed.FileID != "file" {
		t.Fatalf("managed file ID = %q, want file", matches[0].managed.FileID)
	}
}

func TestMatchLibraryFilesResolvesRelativeSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	libraryPath := filepath.Join(dir, "library", "Movie.mkv")
	if err := os.MkdirAll(filepath.Dir(libraryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join("..", "managed", "Movie.Release", "movie.mkv")
	if err := os.Symlink(target, libraryPath); err != nil {
		t.Fatal(err)
	}

	matches, _ := matchLibraryFiles(
		[]arr.LibraryFile{{ArrFileID: 42, Path: libraryPath}},
		[]ManagedFile{{EntryID: "entry", EntryFolder: "Movie.Release", FileID: "file", FileName: "movie.mkv"}},
		filepath.Join(dir, "managed"),
	)
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}
}

func TestMatchLibraryFilesSkipsNonSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	libraryPath := filepath.Join(dir, "Movie.mkv")
	if err := os.WriteFile(libraryPath, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}

	matches, stats := matchLibraryFiles(
		[]arr.LibraryFile{{ArrFileID: 42, Path: libraryPath}},
		[]ManagedFile{{EntryID: "entry", EntryFolder: filepath.Base(dir), FileID: "file", FileName: "Movie.mkv"}},
		dir,
	)
	if len(matches) != 0 {
		t.Fatalf("matches = %d, want 0", len(matches))
	}
	if stats.notSymlink != 1 {
		t.Fatalf("not-symlink = %d, want 1", stats.notSymlink)
	}
}

func TestMatchLibraryFilesRequiresFolderAndFilename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "managed", "Movie.Release", "movie.mkv")
	libraryPath := filepath.Join(dir, "library", "Movie.mkv")
	if err := os.MkdirAll(filepath.Dir(libraryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, libraryPath); err != nil {
		t.Fatal(err)
	}

	matches, stats := matchLibraryFiles(
		[]arr.LibraryFile{{ArrFileID: 42, Path: libraryPath}},
		[]ManagedFile{{EntryID: "entry", EntryFolder: "Other.Release", FileID: "file", FileName: "movie.mkv"}},
		filepath.Join(dir, "managed"),
	)
	if len(matches) != 0 {
		t.Fatalf("matches = %d, want 0", len(matches))
	}
	if stats.unknownEntry != 1 {
		t.Fatalf("unknown-entry = %d, want 1", stats.unknownEntry)
	}
}

func TestMatchLibraryFilesRejectsAmbiguousManagedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "managed", "Movie.Release", "movie.mkv")
	libraryPath := filepath.Join(dir, "library", "Movie.mkv")
	if err := os.MkdirAll(filepath.Dir(libraryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, libraryPath); err != nil {
		t.Fatal(err)
	}

	matches, stats := matchLibraryFiles(
		[]arr.LibraryFile{{ArrFileID: 42, Path: libraryPath}},
		[]ManagedFile{
			{EntryID: "entry-1", EntryFolder: "Movie.Release", FileID: "file-1", FileName: "movie.mkv"},
			{EntryID: "entry-2", EntryFolder: "Movie.Release", FileID: "file-2", FileName: "movie.mkv"},
		},
		filepath.Join(dir, "managed"),
	)
	if len(matches) != 0 {
		t.Fatalf("matches = %d, want 0", len(matches))
	}
	if stats.ambiguousTarget != 1 {
		t.Fatalf("ambiguous-target = %d, want 1", stats.ambiguousTarget)
	}
}

func TestMatchLibraryFilesRejectsDuplicateArrTargets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "managed", "Movie.Release", "movie.mkv")
	libraryDir := filepath.Join(dir, "library")
	if err := os.MkdirAll(libraryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	libraryPaths := []string{
		filepath.Join(libraryDir, "Movie.mkv"),
		filepath.Join(libraryDir, "Movie copy.mkv"),
	}
	for _, path := range libraryPaths {
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}

	matches, stats := matchLibraryFiles(
		[]arr.LibraryFile{
			{ArrFileID: 42, Path: libraryPaths[0]},
			{ArrFileID: 43, Path: libraryPaths[1]},
		},
		[]ManagedFile{{EntryID: "entry", EntryFolder: "Movie.Release", FileID: "file", FileName: "movie.mkv"}},
		filepath.Join(dir, "managed"),
	)
	if len(matches) != 0 {
		t.Fatalf("matches = %d, want 0", len(matches))
	}
	if stats.conflicted != 2 {
		t.Fatalf("conflicted = %d, want 2", stats.conflicted)
	}
}

func TestReconcileBuildsIndexFromSymlinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "managed", "Movie.Release", "movie.mkv")
	libraryPath := filepath.Join(dir, "library", "Movie.mkv")
	regularPath := filepath.Join(dir, "library", "Other.mkv")
	if err := os.MkdirAll(filepath.Dir(libraryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, libraryPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(regularPath, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v3/movie" {
			http.NotFound(w, request)
			return
		}
		_, _ = fmt.Fprintf(
			w,
			`[{"id":9,"movieFile":{"id":42,"path":%q}},{"id":10,"movieFile":{"id":43,"path":%q}}]`,
			libraryPath,
			regularPath,
		)
	}))
	defer server.Close()

	instance := arr.Arr{Name: "radarr", Host: server.URL, Token: "secret", Type: arr.Radarr}
	arrs := arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	arrs.AddOrUpdate(instance)
	writer := new(recordingBindingWriter)
	managed := []ManagedFile{{
		EntryID:     "entry",
		EntryName:   "Movie Release",
		EntryFolder: "Movie.Release",
		FileID:      "file",
		FileName:    "movie.mkv",
		DownloadID:  "download",
	}}
	indexer := &Indexer{
		arrs:        arrs,
		catalog:     fixedManagedCatalog(managed),
		writer:      writer,
		managedRoot: filepath.Join(dir, "managed"),
	}

	stats, err := indexer.reconcile(t.Context(), instance, indexRequest{}, managed)
	if err != nil {
		t.Fatal(err)
	}
	if len(writer.replacement) != 1 {
		t.Fatalf("bindings = %#v", writer.replacement)
	}
	if stats.libraryFiles != 2 || stats.managedFiles != 1 || stats.matchedFolder != 1 || stats.notSymlink != 1 {
		t.Fatalf("stats = %#v", stats)
	}
	binding := writer.replacement[0]
	if binding.EntryID != "entry" || binding.EntryFileID != "file" || binding.ArrFileID != 42 {
		t.Fatalf("binding = %#v", binding)
	}
}

type fixedManagedCatalog []ManagedFile

type failingManagedCatalog struct{}

func (failingManagedCatalog) ListManagedFiles(context.Context, string) ([]ManagedFile, error) {
	return nil, errors.New("catalog unavailable")
}

func (c fixedManagedCatalog) ListManagedFiles(context.Context, string) ([]ManagedFile, error) {
	return c, nil
}

type recordingBindingWriter struct {
	replacement []Binding
	upserts     []Binding
	stored      uint64 // generation reported as already stored
	generation  uint64 // generation of the last replacement
}

func (w *recordingBindingWriter) Generation(string) uint64 { return w.stored }

func (w *recordingBindingWriter) UpsertBinding(binding Binding) error {
	w.upserts = append(w.upserts, binding)
	return nil
}

func (w *recordingBindingWriter) ReplaceArrGeneration(_ string, generation uint64, bindings []Binding) error {
	w.replacement = bindings
	w.generation = generation
	return nil
}

func TestMatchLibraryFilesMatchesRenamedFolderBySize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "managed", "Show.S01.Complete", "episode.mkv")
	libraryPath := filepath.Join(dir, "library", "Episode.mkv")
	if err := os.MkdirAll(filepath.Dir(libraryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, libraryPath); err != nil {
		t.Fatal(err)
	}

	matches, stats := matchLibraryFiles(
		[]arr.LibraryFile{{ArrFileID: 42, Path: libraryPath, Size: 1024}},
		[]ManagedFile{
			{EntryID: "entry", EntryFolder: "Show.S01", FileID: "file", FileName: "episode.mkv", FileSize: 1024},
		},
		filepath.Join(dir, "managed"),
	)
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}
	if matches[0].confidence != ConfidenceManagedTarget {
		t.Fatalf("confidence = %q, want %q", matches[0].confidence, ConfidenceManagedTarget)
	}
	if stats.matchedSize != 1 || stats.matchedFolder != 0 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestMatchLibraryFilesSizeMatchNeedsUniqueName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "managed", "Show.S01.Complete", "episode.mkv")
	libraryPath := filepath.Join(dir, "library", "Episode.mkv")
	if err := os.MkdirAll(filepath.Dir(libraryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, libraryPath); err != nil {
		t.Fatal(err)
	}

	matches, stats := matchLibraryFiles(
		[]arr.LibraryFile{{ArrFileID: 42, Path: libraryPath, Size: 1024}},
		[]ManagedFile{
			{EntryID: "entry-1", EntryFolder: "Show.S01", FileID: "file-1", FileName: "episode.mkv", FileSize: 1024},
			{
				EntryID:     "entry-2",
				EntryFolder: "Show.S01.Repack",
				FileID:      "file-2",
				FileName:    "episode.mkv",
				FileSize:    1024,
			},
		},
		filepath.Join(dir, "managed"),
	)
	if len(matches) != 0 {
		t.Fatalf("matches = %d, want 0", len(matches))
	}
	if stats.ambiguousTarget != 1 {
		t.Fatalf("ambiguous_target = %d, want 1", stats.ambiguousTarget)
	}
}

func TestMatchLibraryFilesSkipsTargetsOutsideMount(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere", "Movie.Release", "movie.mkv")
	libraryPath := filepath.Join(dir, "library", "Movie.mkv")
	if err := os.MkdirAll(filepath.Dir(libraryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, libraryPath); err != nil {
		t.Fatal(err)
	}

	matches, stats := matchLibraryFiles(
		[]arr.LibraryFile{{ArrFileID: 42, Path: libraryPath, Size: 1024}},
		[]ManagedFile{
			{EntryID: "entry", EntryFolder: "Movie.Release", FileID: "file", FileName: "movie.mkv", FileSize: 1024},
		},
		filepath.Join(dir, "managed"),
	)
	if len(matches) != 0 {
		t.Fatalf("matches = %d, want 0", len(matches))
	}
	if stats.foreignTarget != 1 || stats.unknownEntry != 0 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestMatchLibraryFilesFolderMatchWinsOverSizeMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := filepath.Join(dir, "managed")
	libraryDir := filepath.Join(dir, "library")
	if err := os.MkdirAll(libraryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exactPath := filepath.Join(libraryDir, "Exact.mkv")
	driftedPath := filepath.Join(libraryDir, "Drifted.mkv")
	if err := os.Symlink(filepath.Join(root, "Show.S01", "episode.mkv"), exactPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "Show.S01.Complete", "episode.mkv"), driftedPath); err != nil {
		t.Fatal(err)
	}

	matches, stats := matchLibraryFiles(
		[]arr.LibraryFile{
			{ArrFileID: 42, Path: exactPath, Size: 1024},
			{ArrFileID: 43, Path: driftedPath, Size: 1024},
		},
		[]ManagedFile{
			{EntryID: "entry", EntryFolder: "Show.S01", FileID: "file", FileName: "episode.mkv", FileSize: 1024},
		},
		root,
	)
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}
	if matches[0].library.ArrFileID != 42 || matches[0].confidence != ConfidenceExactPath {
		t.Fatalf("match = %#v", matches[0])
	}
	if stats.matchedFolder != 1 || stats.conflicted != 1 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestMatchLibraryFilesMatchesNestedMountTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := filepath.Join(dir, "managed")
	target := filepath.Join(root, "Show.S01", "Season 1", "episode.mkv")
	libraryPath := filepath.Join(dir, "library", "Episode.mkv")
	if err := os.MkdirAll(filepath.Dir(libraryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, libraryPath); err != nil {
		t.Fatal(err)
	}

	matches, stats := matchLibraryFiles(
		[]arr.LibraryFile{{ArrFileID: 42, Path: libraryPath}},
		[]ManagedFile{{EntryID: "entry", EntryFolder: "Show.S01", FileID: "file", FileName: "episode.mkv"}},
		root,
	)
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}
	if stats.matchedFolder != 1 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestMatchLibraryFilesSeparatesUnknownEntryFromUnknownFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := filepath.Join(dir, "managed")
	libraryDir := filepath.Join(dir, "library")
	if err := os.MkdirAll(libraryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	knownEntry := filepath.Join(libraryDir, "Known.mkv")
	goneEntry := filepath.Join(libraryDir, "Gone.mkv")
	if err := os.Symlink(filepath.Join(root, "Show.S01", "other.mkv"), knownEntry); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "Deleted.S01", "episode.mkv"), goneEntry); err != nil {
		t.Fatal(err)
	}

	_, stats := matchLibraryFiles(
		[]arr.LibraryFile{
			{ArrFileID: 42, Path: knownEntry},
			{ArrFileID: 43, Path: goneEntry},
		},
		[]ManagedFile{{EntryID: "entry", EntryFolder: "Show.S01", FileID: "file", FileName: "episode.mkv"}},
		root,
	)
	if stats.unknownFile != 1 || stats.unknownEntry != 1 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestReconcileTargetedNeverWidensToTheWholeLibrary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		if request.URL.Path != "/api/v3/history" {
			t.Errorf("unexpected request to %q", request.URL.Path)
			http.NotFound(w, request)
			return
		}
		_, _ = fmt.Fprint(w, `{"page":1,"pageSize":100,"totalRecords":0,"records":[]}`)
	}))
	defer server.Close()

	instance := arr.Arr{Name: "radarr", Host: server.URL, Token: "secret", Type: arr.Radarr}
	arrs := arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	arrs.AddOrUpdate(instance)
	managed := []ManagedFile{{
		EntryID:     "entry",
		EntryFolder: "Movie.Release",
		FileID:      "file",
		FileName:    "movie.mkv",
		DownloadID:  "download",
	}}
	indexer := &Indexer{
		arrs:        arrs,
		catalog:     fixedManagedCatalog(managed),
		writer:      new(recordingBindingWriter),
		managedRoot: filepath.Join(dir, "managed"),
	}

	request := indexRequest{arrName: "radarr", entryID: "entry", attempt: targetedIndexAttempts}
	stats, err := indexer.reconcile(t.Context(), instance, request, managed)
	if err != nil {
		t.Fatal(err)
	}
	if stats.matched() != 0 {
		t.Fatalf("stats = %#v, want no match", stats)
	}
	if slices.Contains(paths, "/api/v3/movie") {
		t.Fatalf("targeted attempt listed the whole library: %v", paths)
	}
	if !slices.Contains(paths, "/api/v3/history") {
		t.Fatalf("targeted attempt did not read history: %v", paths)
	}
}

func TestExhaustedTargetedRequestsCoalesceArrRefresh(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	indexer := NewIndexer(nil, nil, nil, "", zerolog.Nop())
	indexer.ctx = ctx

	finalAttempt := targetedIndexAttempts
	indexer.retryTargeted(ctx, indexRequest{arrName: "radarr", entryID: "one", attempt: finalAttempt}, false, nil)
	indexer.retryTargeted(ctx, indexRequest{arrName: "radarr", entryID: "two", attempt: finalAttempt}, false, nil)

	indexer.mu.Lock()
	queue := slices.Clone(indexer.queue)
	indexer.mu.Unlock()
	if len(queue) != 1 {
		t.Fatalf("queued %d refreshes, want 1: %#v", len(queue), queue)
	}
	if queue[0].arrName != "radarr" || queue[0].entryID != "" {
		t.Fatalf("request = %#v, want an authoritative Radarr refresh", queue[0])
	}
}

func TestRefreshCoverageSkipsOnlyOlderTargetedRequests(t *testing.T) {
	t.Parallel()
	indexer := NewIndexer(nil, nil, nil, "", zerolog.Nop())
	indexer.markCovered("radarr", 10)

	if !indexer.coveredByRefresh(indexRequest{arrName: "radarr", entryID: "old", version: 10}) {
		t.Fatal("refresh did not cover an older targeted request")
	}
	if indexer.coveredByRefresh(indexRequest{arrName: "radarr", entryID: "new", version: 11}) {
		t.Fatal("refresh covered a targeted request created after its snapshot")
	}
	if indexer.coveredByRefresh(indexRequest{arrName: "sonarr", entryID: "other", version: 1}) {
		t.Fatal("Radarr refresh covered a Sonarr request")
	}
}

// A new generation is always newer than the stored one, even when the clock
// has stepped back: an older generation would be discarded as superseded the
// next time bindings load.
func TestReconcileGenerationOutrunsStoredGeneration(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer server.Close()

	instance := arr.Arr{Name: "radarr", Host: server.URL, Token: "secret", Type: arr.Radarr}
	arrs := arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	arrs.AddOrUpdate(instance)
	stored := uint64(time.Now().Add(time.Hour).UnixMilli()) // written before the clock went back
	writer := &recordingBindingWriter{stored: stored}
	indexer := &Indexer{arrs: arrs, catalog: fixedManagedCatalog(nil), writer: writer}

	if _, err := indexer.reconcile(t.Context(), instance, indexRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	if writer.generation <= stored {
		t.Fatalf("generation %d is not newer than the stored %d", writer.generation, stored)
	}
}

// With the arr's import webhook the only timer is one outer bound; without it the history poll keeps its schedule.
func TestImportWaitSchedule(t *testing.T) {
	t.Parallel()
	webhook := []time.Duration{30 * time.Minute}
	timer := []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second}
	for range targetedIndexAttempts - len(timer) {
		timer = append(timer, targetedIndexSteadyDelay)
	}
	for name, want := range map[bool][]time.Duration{true: webhook, false: timer} {
		for attempt, delay := range want {
			if got, ok := importWaitDelay(name, indexRequest{attempt: attempt}); !ok || got != delay {
				t.Errorf("webhook=%v: importWaitDelay(%d) = %s, %v; want %s", name, attempt, got, ok, delay)
			}
		}
		if _, ok := importWaitDelay(name, indexRequest{attempt: len(want), lateWaited: name}); ok {
			t.Errorf("webhook=%v: still waiting after %d attempts; want the full refresh", name, len(want))
		}
	}
}

// With the webhook on, an entry still unmatched after the late wait goes to the full refresh; without it the timer
// keeps polling. A retry after a failed attempt has not spent the late wait yet.
func TestUnmatchedEntryAfterLateWait(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		webhook, lateWaited, wantRefresh bool
	}{
		{webhook: true, lateWaited: true, wantRefresh: true},
		{webhook: true, lateWaited: false, wantRefresh: false},
		{webhook: false, lateWaited: false, wantRefresh: false},
	} {
		t.Run(fmt.Sprintf("webhook=%v/lateWaited=%v", tc.webhook, tc.lateWaited), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, `{"page":1,"pageSize":100,"totalRecords":0,"records":[]}`)
			}))
			t.Cleanup(server.Close)
			arrs := arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
			arrs.AddOrUpdate(arr.Arr{
				Name: "radarr", Host: server.URL, Token: "secret", Type: arr.Radarr, ImportWebhook: tc.webhook,
			})
			managed := []ManagedFile{{EntryID: "entry", EntryFolder: "Movie", FileID: "file", FileName: "movie.mkv"}}
			writer := new(recordingBindingWriter)
			var logs bytes.Buffer
			indexer := NewIndexer(arrs, fixedManagedCatalog(managed), writer, t.TempDir(), zerolog.New(&logs))
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			indexer.ctx = ctx

			indexer.handle(
				ctx,
				indexRequest{arrName: "radarr", entryID: "entry", attempt: 1, lateWaited: tc.lateWaited},
			)

			indexer.mu.Lock()
			queue := slices.Clone(indexer.queue)
			indexer.mu.Unlock()
			if refreshed := len(queue) == 1 && queue[0].entryID == ""; refreshed != tc.wantRefresh {
				t.Errorf("queue = %#v; want full refresh = %v", queue, tc.wantRefresh)
			}
			if tripped := strings.Contains(logs.String(), "wait ran out"); tripped != tc.wantRefresh {
				t.Errorf("logs = %s; want the trip logged = %v", logs.String(), tc.wantRefresh)
			}
		})
	}
}

// Without the webhook a targeted attempt reads the library only once history records the import; with it, the
// webhook is the signal and may arrive before the arr writes its history.
func TestTargetedIndexWaitsForTheImportInHistory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		webhook   bool
		event     string
		wantMatch bool
	}{
		{webhook: false, event: "grabbed", wantMatch: false},
		{webhook: false, event: "downloadFolderImported", wantMatch: true},
		{webhook: true, event: "grabbed", wantMatch: true},
	} {
		t.Run(fmt.Sprintf("webhook=%v/%s", tc.webhook, tc.event), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			libraryPath := filepath.Join(dir, "library", "Movie.mkv")
			if err := os.MkdirAll(filepath.Dir(libraryPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(dir, "managed", "Movie.Release", "movie.mkv"), libraryPath); err != nil {
				t.Fatal(err)
			}
			var movieReads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api/v3/history":
					_, _ = fmt.Fprintf(w, `{"page":1,"pageSize":100,"totalRecords":1,`+
						`"records":[{"id":1,"downloadId":"download","eventType":%q,"movieId":9}]}`, tc.event)
				case "/api/v3/movie/9":
					movieReads.Add(1)
					_, _ = fmt.Fprintf(w, `{"id":9,"movieFile":{"id":42,"movieId":9,"path":%q}}`, libraryPath)
				default:
					t.Errorf("unexpected request to %q", request.URL.Path)
					http.NotFound(w, request)
				}
			}))
			t.Cleanup(server.Close)

			instance := arr.Arr{
				Name: "radarr", Host: server.URL, Token: "secret", Type: arr.Radarr, ImportWebhook: tc.webhook,
			}
			arrs := arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
			arrs.AddOrUpdate(instance)
			managed := []ManagedFile{
				{
					EntryID:     "entry",
					EntryFolder: "Movie.Release",
					FileID:      "file",
					FileName:    "movie.mkv",
					DownloadID:  "download",
				},
			}
			writer := new(recordingBindingWriter)
			indexer := &Indexer{
				arrs:        arrs,
				catalog:     fixedManagedCatalog(managed),
				writer:      writer,
				managedRoot: filepath.Join(dir, "managed"),
			}

			stats, err := indexer.reconcile(
				t.Context(),
				instance,
				indexRequest{arrName: "radarr", entryID: "entry"},
				managed,
			)
			if err != nil {
				t.Fatal(err)
			}
			got := [3]bool{stats.matched() == 1, movieReads.Load() > 0, len(writer.upserts) == 1}
			if want := [3]bool{tc.wantMatch, tc.wantMatch, tc.wantMatch}; got != want {
				t.Errorf("matched, library read, binding written = %v; want %v", got, want)
			}
		})
	}
}

// The webhook's outer bound is spent once: the request it wakes goes to the full refresh if it still misses.
func TestWebhookWaitRunsOutOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		indexer := NewIndexer(nil, nil, nil, "", zerolog.Nop())
		indexer.ctx = ctx

		indexer.retryTargeted(ctx, indexRequest{arrName: "radarr", entryID: "entry"}, true, nil)
		time.Sleep(lateImportWait)
		synctest.Wait()
		request, ok := indexer.nextRequest()
		if !ok || !request.lateWaited {
			t.Fatalf("request after the wait = %#v, %v; want the entry, marked as having waited", request, ok)
		}

		indexer.retryTargeted(ctx, request, true, nil)
		if refresh, queued := indexer.nextRequest(); !queued || refresh.entryID != "" {
			t.Fatalf("request = %#v, %v; want the full refresh", refresh, queued)
		}
		cancel()
		indexer.wg.Wait()
	})
}

// The arr's history filter is exact: torrent IDs (infohashes, season entries) are stored uppercased, usenet IDs as
// given, and a usenet season entry's ID is hex like a torrent's.
func TestTargetedIndexQueriesHistoryByTheArrsDownloadID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, managed, stored string }{
		{name: "torrent", managed: "8eae87348844aabbccddeeff0011223344556677", stored: "8EAE87348844AABBCCDDEEFF0011223344556677"},
		{name: "season", managed: "0a1b2c3d4e5f60718293a4b5c6d7e8f9", stored: "0A1B2C3D4E5F60718293A4B5C6D7E8F9"},
		{name: "usenet", managed: "9fdb8e00-5a38-4c1e-9d3b-0a1b2c3d4e5f", stored: "9fdb8e00-5a38-4c1e-9d3b-0a1b2c3d4e5f"},
		{name: "usenet season", managed: "0a1b2c3d4e5f60718293a4b5c6d7e8f9", stored: "0a1b2c3d4e5f60718293a4b5c6d7e8f9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			libraryPath := filepath.Join(dir, "library", "Movie.mkv")
			if err := os.MkdirAll(filepath.Dir(libraryPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(dir, "managed", "Movie.Release", "movie.mkv"), libraryPath); err != nil {
				t.Fatal(err)
			}
			server := exactHistoryRadarr(t, tc.stored, libraryPath)

			instance := arr.Arr{Name: "radarr", Host: server.URL, Token: "secret", Type: arr.Radarr}
			arrs := arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
			arrs.AddOrUpdate(instance)
			managed := []ManagedFile{{
				EntryID:     "entry",
				EntryFolder: "Movie.Release",
				FileID:      "file",
				FileName:    "movie.mkv",
				DownloadID:  tc.managed,
			}}
			indexer := &Indexer{
				arrs:        arrs,
				catalog:     fixedManagedCatalog(managed),
				writer:      new(recordingBindingWriter),
				managedRoot: filepath.Join(dir, "managed"),
			}

			request := indexRequest{arrName: "radarr", entryID: "entry"}
			stats, err := indexer.reconcile(t.Context(), instance, request, managed)
			if err != nil {
				t.Fatal(err)
			}
			if stats.matched() != 1 {
				t.Fatalf("stats = %#v, want the entry matched via history", stats)
			}
		})
	}
}

// exactHistoryRadarr answers history only for downloadID exactly, like the arr's case-sensitive filter.
func exactHistoryRadarr(t *testing.T, downloadID, libraryPath string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v3/history" && request.URL.Query().Get("downloadId") == downloadID:
			_, _ = fmt.Fprintf(w, `{"page":1,"pageSize":100,"totalRecords":1,`+
				`"records":[{"id":1,"downloadId":%q,"eventType":"downloadFolderImported","movieId":9}]}`, downloadID)
		case request.URL.Path == "/api/v3/history":
			_, _ = fmt.Fprint(w, `{"page":1,"pageSize":100,"totalRecords":0,"records":[]}`)
		case request.URL.Path == "/api/v3/movie/9":
			_, _ = fmt.Fprintf(w, `{"id":9,"movieFile":{"id":42,"movieId":9,"path":%q}}`, libraryPath)
		default:
			t.Errorf("unexpected request to %q", request.URL.Path)
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// A failed attempt retries on the poll schedule even with the webhook, whose outer bound it has not spent, and logs
// the arr's real webhook setting when the retries run out.
func TestFailedTargetedAttemptRetriesOnThePollSchedule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		request     indexRequest
		wantRefresh bool
	}{
		{name: "retries left after the late wait", request: indexRequest{attempt: 1, lateWaited: true}},
		{name: "retries run out", request: indexRequest{attempt: targetedIndexAttempts}, wantRefresh: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			arrs := arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
			arrs.AddOrUpdate(arr.Arr{
				Name: "radarr", Host: "http://arr.invalid", Token: "secret", Type: arr.Radarr, ImportWebhook: true,
			})
			var logs bytes.Buffer
			writer := new(recordingBindingWriter)
			indexer := NewIndexer(arrs, failingManagedCatalog{}, writer, t.TempDir(), zerolog.New(&logs))
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(func() { cancel(); indexer.wg.Wait() })
			indexer.ctx = ctx

			request := tc.request
			request.arrName, request.entryID = "radarr", "entry"
			indexer.handle(ctx, request)

			if !strings.Contains(logs.String(), "attempt failed") {
				t.Fatalf("logs = %s; want the failed attempt", logs.String())
			}
			indexer.mu.Lock()
			queue := slices.Clone(indexer.queue)
			indexer.mu.Unlock()
			if refreshed := len(queue) == 1 && queue[0].entryID == ""; refreshed != tc.wantRefresh {
				t.Errorf("queue = %#v; want full refresh = %v", queue, tc.wantRefresh)
			}
			tripped := strings.Contains(logs.String(), "wait ran out")
			if tripped != tc.wantRefresh {
				t.Errorf("logs = %s; want the trip logged = %v", logs.String(), tc.wantRefresh)
			}
			if tripped && !strings.Contains(logs.String(), `"import_webhook":true`) {
				t.Errorf("logs = %s; want import_webhook=true", logs.String())
			}
		})
	}
}

// The webhook's own request supersedes the outer bound still pending for the same entry.
func TestReindexEntryEndsThePendingWait(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		indexer := NewIndexer(nil, nil, nil, "", zerolog.Nop())
		indexer.ctx = ctx

		indexer.retryTargeted(ctx, indexRequest{arrName: "radarr", entryID: "entry"}, true, nil)
		if !indexer.ReindexEntry("radarr", "entry") {
			t.Fatal("ReindexEntry refused the webhook's request")
		}
		if request, ok := indexer.nextRequest(); !ok || request.lateWaited {
			t.Fatalf("request = %#v, %v; want the webhook's request", request, ok)
		}

		time.Sleep(lateImportWait)
		synctest.Wait()
		if request, ok := indexer.nextRequest(); ok {
			t.Fatalf("request = %#v; want the superseded wait dropped", request)
		}
		cancel()
		indexer.wg.Wait()
	})
}
