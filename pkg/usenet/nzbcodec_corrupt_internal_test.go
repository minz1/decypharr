package usenet

import (
	"encoding/binary"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// decodeAllPaths runs every v2 decode entry point and fails the test if any
// accepts the blob; a panic fails the test on its own.
func decodeAllPaths(t *testing.T, blob []byte) {
	t.Helper()
	if _, err := decodeNZB(blob); err == nil {
		t.Error("decodeNZB accepted a corrupt blob")
	}
	if _, err := decodeFileV2(blob, "movie.mkv"); err == nil {
		t.Error("decodeFileV2 accepted a corrupt blob")
	}
	if _, _, err := decodeFileMessageIDsSampled(blob, "movie.mkv", 100); err == nil {
		t.Error("decodeFileMessageIDsSampled accepted a corrupt blob")
	}
}

func TestDecodeRejectsOverflowingRegionLength(t *testing.T) {
	t.Parallel()
	blob := binary.AppendUvarint([]byte{codecMagicV2}, ^uint64(0))
	decodeAllPaths(t, blob)
	if _, err := decodeNZBV2Header(blob); err == nil {
		t.Error("decodeNZBV2Header accepted a corrupt blob")
	}
}

func TestDecodeRejectsImpossibleSegmentCount(t *testing.T) {
	t.Parallel()
	nzb := &storage.NZB{ID: "id", Files: []storage.NZBFile{{Name: "movie.mkv"}}}
	header := encodeHeader(nzb)
	// The trailing uvarint is the only file's segment count (0); claim 2^40.
	header = binary.AppendUvarint(header[:len(header)-1], 1<<40)
	segMeta, msgIDs := encodeSegments(nzb)

	hc := zstdEnc.EncodeAll(header, nil)
	sc := zstdEnc.EncodeAll(segMeta, nil)
	blob := binary.AppendUvarint([]byte{codecMagicV2}, uint64(len(hc)))
	blob = append(blob, hc...)
	blob = binary.AppendUvarint(blob, uint64(len(sc)))
	blob = append(blob, sc...)
	blob = append(blob, zstdEnc.EncodeAll(msgIDs, nil)...)
	decodeAllPaths(t, blob)
}
