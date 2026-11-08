package reacquire

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
)

// fakeSonarrRig is a service over a fakeSonarr with one command-search job
// queued for the fake's file. Tests drive the job with runJob.
type fakeSonarrRig struct {
	fake      *fakeSonarr
	directory string
	arrs      *arr.Service
	service   *Service
	handler   Handler
	job       Job
	skew      atomic.Int64
	announced attentionLog
	logs      lockedBuffer
}

// lockedBuffer is a log sink the service's goroutines and the test can both use.
type lockedBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (buffer *lockedBuffer) Write(p []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.data.Write(p)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.data.String()
}

// attentionLog collects the needs-attention notices a service raised.
type attentionLog struct {
	mu      sync.Mutex
	notices []NeedsAttention
}

func (log *attentionLog) add(notice NeedsAttention) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.notices = append(log.notices, notice)
}

func (log *attentionLog) all() []NeedsAttention {
	log.mu.Lock()
	defer log.mu.Unlock()
	return slices.Clone(log.notices)
}

func newFakeSonarrRig(t *testing.T, fake *fakeSonarr, strategy Strategy, episodeIDs ...int) *fakeSonarrRig {
	t.Helper()
	rig := &fakeSonarrRig{fake: fake, directory: t.TempDir()}
	rig.arrs = arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	rig.arrs.AddOrUpdate(fake.instance())
	rig.handler = NewHandler(rig.arrs, nil)
	rig.open(t)
	binding := fakeSonarrBinding(fake.instance(), episodeIDs...)
	if err := rig.service.UpsertBinding(binding); err != nil {
		t.Fatal(err)
	}
	job, err := rig.service.Reacquire(Request{
		EntryID:  binding.EntryID,
		FileID:   binding.EntryFileID,
		Cause:    CauseManual,
		Strategy: strategy,
	})
	if err != nil {
		t.Fatal(err)
	}
	rig.job = *job
	return rig
}

// open starts the service on the rig's directory, as a restart would.
func (rig *fakeSonarrRig) open(t *testing.T) {
	t.Helper()
	service, err := NewService(newTestGroup(t), ServiceOptions{
		Arrs: rig.arrs, Directory: rig.directory, OnNeedsAttention: rig.announced.add,
		Logger: zerolog.New(&rig.logs).Level(zerolog.DebugLevel),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	service.now = func() time.Time { return time.Now().UTC().Add(time.Duration(rig.skew.Load())) }
	if startErr := service.Start(t.Context()); startErr != nil {
		t.Fatal(startErr)
	}
	rig.service = service
}

// advance moves the service's clock forward.
func (rig *fakeSonarrRig) advance(by time.Duration) {
	rig.skew.Add(int64(by))
}

// run executes the job once and returns its stored state.
func (rig *fakeSonarrRig) run(t *testing.T) Job {
	t.Helper()
	current, ok := rig.service.Job(rig.job.ID)
	if !ok {
		t.Fatal("job is gone")
	}
	if !rig.service.runJob(t.Context(), rig.handler, current) {
		t.Fatal("runJob could not save the job")
	}
	settled, _ := rig.service.Job(rig.job.ID)
	return settled
}

func TestUnknownFileDeleteRequeuesAndContinuesOnceTheFileIsGone(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	fake.setDeleteMode("drop-landed")
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)

	queued := rig.run(t)
	if queued.Status != StatusQueued || queued.RetryAt.IsZero() {
		t.Fatalf("after an unknown delete the job is %s (retry at %v): %s",
			queued.Status, queued.RetryAt, queued.LastError)
	}

	waiting := rig.run(t)
	if deletes, searches := fake.deleteCount(), fake.searchCount(); deletes != 1 || searches != 1 {
		t.Fatalf("deletes = %d, searches = %d, want 1 and 1", deletes, searches)
	}
	if waiting.Status != StatusWaitingForGrab {
		t.Fatalf("job = %s: %s", waiting.Status, waiting.LastError)
	}
}

func TestFileLookupFailureIsRetriedNotFatal(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	handler := newTestHandler(server.URL)
	binding := Binding{
		ArrName: "movies", ArrType: arr.Radarr, ArrFileID: 42, MovieID: 9, LibraryPath: "/library/movie.mkv",
	}

	err := handler.deleteArrFiles(
		t.Context(), testInstance(server.URL), &Job{}, []Binding{binding}, &recordedProgress{},
	)
	if !errors.Is(err, errArrLookup) {
		t.Fatalf("deleteArrFiles() = %v, want a lookup error the worker retries", err)
	}
}
