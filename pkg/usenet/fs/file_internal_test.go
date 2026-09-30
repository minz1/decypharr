package fs

import (
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/types"
)

func TestReadAtSurfacesReaderCreationError(t *testing.T) {
	t.Parallel()
	file := &File{
		readerSettings: readerSettings{logger: zerolog.Nop()},
		volume: &types.Volume{
			Name:     "movie.mkv",
			Size:     10,
			Segments: []storage.NZBSegment{{MessageID: "<a@b>", Bytes: 10, EndOffset: 9}},
		},
	}
	_, err := file.ReadAt(make([]byte, 4), 0)
	if err == nil || !strings.Contains(err.Error(), "no connection client") {
		t.Fatalf("ReadAt error = %v, want the reader construction cause", err)
	}
}
