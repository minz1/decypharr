package usenet

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// A slow segment holds back writing, not fetching: without a bound every
// later segment is fetched and held in RAM until it lands. Fetching stops
// one window past the stalled segment and resumes once it is written.
func TestDownloadSegmentsBoundsReadAhead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const workers, total = 2, 200
		window := workers * downloadWindowPerWorker
		segments := make([]storage.NZBSegment, total)
		release := make(chan struct{})
		var fetched atomic.Int64
		fetch := func(_ context.Context, idx int, _ storage.NZBSegment) segmentResult {
			fetched.Add(1)
			if idx == 0 {
				<-release
			}
			return segmentResult{index: idx, data: []byte{byte(idx)}}
		}

		var out bytes.Buffer
		done := make(chan error, 1)
		go func() {
			_, err := downloadSegments(t.Context(), segments, workers, &out, nil, fetch)
			done <- err
		}()
		synctest.Wait()
		if got := fetched.Load(); got > int64(window) {
			t.Errorf("fetched %d segments while segment 0 stalled, want at most %d", got, window)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if out.Len() != total {
			t.Fatalf("wrote %d bytes, want %d", out.Len(), total)
		}
		for i, b := range out.Bytes() {
			if int(b) != i%256 {
				t.Fatalf("byte %d = %d, segments written out of order", i, b)
			}
		}
	})
}
