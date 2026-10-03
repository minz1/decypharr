package usenet

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// The availability and content gates skip their checks once the context is
// done. An interrupted run must not then record the NZB as completed.
func TestFinishFailsInterruptedNZB(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nzbStorage, err := NewNZBStorage(filepath.Join(dir, "meta"), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	u := &Usenet{
		config:     config.NewStore(config.New(dir)),
		nzbStorage: nzbStorage,
		logger:     zerolog.Nop(),
	}
	nzb := &storage.NZB{
		ID:     "interrupted",
		Name:   "Movie",
		Status: NZBStatusParsing,
		Files: []storage.NZBFile{{
			Name:     "Movie.mkv",
			Segments: []storage.NZBSegment{{MessageID: "a@b", Bytes: 1}},
		}},
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if finishErr := u.finish(ctx, nzb); !errors.Is(finishErr, context.Canceled) {
		t.Fatalf("finish = %v, want context.Canceled", finishErr)
	}
	stored, err := nzbStorage.GetNZB(nzb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != NZBStatusFailed {
		t.Fatalf("interrupted NZB stored as %q, want %q", stored.Status, NZBStatusFailed)
	}
}
