package usenet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/manifest"
	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
)

// blockingSource parks every header fetch until its context ends, as a stalled
// provider does while the process is being shut down.
type blockingSource struct {
	parser.ArticleSource

	entered chan struct{}
}

func (s *blockingSource) Header(ctx context.Context, _ string, _ int) (*nntp.YencMetadata, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *blockingSource) Metrics() parser.ArticleMetrics { return parser.ArticleMetrics{} }

// parsingNZB writes a parsing record with its source and marker, the state an
// NZB is in while Process runs.
func parsingNZB(t *testing.T, id string) (*Usenet, *storage.NZB, string) {
	t.Helper()
	u, source := newMarkedUsenet(t, id)
	nzb := &storage.NZB{ID: id, Name: "Movie", Path: source, Status: NZBStatusParsing}
	if err := u.nzbStorage.AddNZB(nzb); err != nil {
		t.Fatal(err)
	}
	return u, nzb, source
}

func assertStillParsing(t *testing.T, u *Usenet, nzb *storage.NZB, source string) {
	t.Helper()
	header, err := u.nzbStorage.GetNZBHeader(nzb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if header.Status != NZBStatusParsing {
		t.Fatalf("persisted status = %q, want %q so a restart re-drives it", header.Status, NZBStatusParsing)
	}
	for _, p := range []string{source, source + ".processing"} {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Errorf("%s must survive an interrupted run: %v", filepath.Base(p), statErr)
		}
	}
}

// Shutdown cancels the context mid-Process. That says nothing about the NZB, so
// it must stay parsing for the restore to re-drive instead of being failed.
func TestProcessCancelledMidParseLeavesNZBParsing(t *testing.T) {
	t.Parallel()
	u, nzb, source := parsingNZB(t, "cancelled")
	source2 := &blockingSource{entered: make(chan struct{}, 1)}
	u.analyzer = parser.NewParserWithSource(source2, 1, zerolog.Nop())
	groups := map[string]*parser.FileGroup{"movie": {
		BaseName: "movie",
		Type:     storage.NZBFileTypeMedia,
		Files: []manifest.File{{
			Filename: "movie.mkv", Bytes: 100, TotalSegments: 1,
			Segments: []manifest.Segment{{Number: 1, Bytes: 100, MessageID: "a@b"}},
		}},
	}}

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := u.Process(ctx, nzb, groups)
		result <- err
	}()
	select {
	case <-source2.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Process never reached the provider")
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Process = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Process ignored the cancellation")
	}
	assertStillParsing(t, u, nzb, source)
}

// The availability and content gates skip their checks once the context is
// done. An interrupted run must neither complete nor fail the NZB.
func TestFinishLeavesCancelledNZBParsing(t *testing.T) {
	t.Parallel()
	u, nzb, source := parsingNZB(t, "interrupted")
	u.config = config.NewStore(config.New(t.TempDir()))
	nzb.Files = []storage.NZBFile{{
		Name:     "Movie.mkv",
		Segments: []storage.NZBSegment{{MessageID: "a@b", Bytes: 1}},
	}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if finishErr := u.finish(ctx, nzb); !errors.Is(finishErr, context.Canceled) {
		t.Fatalf("finish = %v, want context.Canceled", finishErr)
	}
	assertStillParsing(t, u, nzb, source)
}

// A run that outlives its own time budget is a real failure of that NZB, not a
// shutdown, and is still recorded as one.
func TestFinishFailsNZBThatTimedOut(t *testing.T) {
	t.Parallel()
	u, nzb, _ := parsingNZB(t, "timed-out")
	u.config = config.NewStore(config.New(t.TempDir()))
	nzb.Files = []storage.NZBFile{{
		Name:     "Movie.mkv",
		Segments: []storage.NZBSegment{{MessageID: "a@b", Bytes: 1}},
	}}
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()

	if finishErr := u.finish(ctx, nzb); !errors.Is(finishErr, context.DeadlineExceeded) {
		t.Fatalf("finish = %v, want context.DeadlineExceeded", finishErr)
	}
	header, err := u.nzbStorage.GetNZBHeader(nzb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if header.Status != NZBStatusFailed {
		t.Fatalf("timed-out NZB stored as %q, want %q", header.Status, NZBStatusFailed)
	}
}
