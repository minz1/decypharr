package parser

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/manifest"
)

// joinSource fails the reference file's header at once and holds the last
// file's header until its ctx ends, recording that the fetch returned.
type joinSource struct {
	headerSource

	lastReturned *atomic.Bool
}

func (s joinSource) Header(ctx context.Context, id string, _ int) (*nntp.YencMetadata, error) {
	if id != "last@x" {
		return nil, errors.New("reference header broken")
	}
	<-ctx.Done()
	s.lastReturned.Store(true)
	return nil, ctx.Err()
}

// An early return from enrich (the reference header failed) must cancel and
// join the last file's header fetch: no fetch outlives the call. Without the
// cancel the join never ends, so the test also pins the cancel.
func TestEnrichJoinsHeaderFetchesOnEarlyReturn(t *testing.T) {
	t.Parallel()
	var lastReturned atomic.Bool
	p := NewParserWithSource(joinSource{lastReturned: &lastReturned}, 1, zerolog.Nop())
	group := &FileGroup{
		BaseName: "movie",
		Type:     storage.NZBFileTypeMedia,
		fileMeta: map[string]filePartMeta{},
		Files: []manifest.File{
			{Filename: "movie.mkv", Number: 1, Segments: []manifest.Segment{
				{Number: 1, MessageID: "first-1@x"}, {Number: 2, MessageID: "first-2@x"},
			}},
			{Filename: "movie.mkv", Number: 2, Segments: []manifest.Segment{
				{Number: 1, MessageID: "last@x"},
			}},
		},
	}

	if err := p.enrichGroupWithFileInfo(t.Context(), group); err == nil {
		t.Fatal("enrich succeeded with a broken reference header")
	}
	if !lastReturned.Load() {
		t.Fatal("enrich returned while the last file's header fetch was still running")
	}
}
