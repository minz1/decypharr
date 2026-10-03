package usenet

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// buildCodecNZB returns an NZB with uneven per-file segment counts, a deleted
// file and several groups, so single-file decoding has to skip the right
// number of column entries in every direction.
func buildCodecNZB() *storage.NZB {
	nzb := &storage.NZB{
		ID:         "nzb-1",
		Name:       "Some.Release.2160p",
		Title:      "Some Release",
		Path:       "/meta/nzb-1.meta",
		Category:   "radarr",
		Groups:     []string{"alt.binaries.a", "alt.binaries.b"},
		DatePosted: time.Unix(1700000000, 0).UTC(),
		AddedOn:    time.Unix(1700000100, 0).UTC(),
	}
	counts := []int{3, 0, 7, 1, 5}
	var offset int64
	for i, count := range counts {
		file := storage.NZBFile{
			Name:        fmt.Sprintf("file-%d.mkv", i),
			Groups:      []string{"alt.binaries.a"},
			SegmentSize: 750000,
			IsDeleted:   i == 3,
		}
		for j := range count {
			file.Segments = append(file.Segments, storage.NZBSegment{
				Number:           j + 1,
				Bytes:            int64(700000 + j),
				StartOffset:      offset,
				EndOffset:        offset + 700000,
				SegmentDataStart: int64(j * 3),
				Group:            nzb.Groups[j%len(nzb.Groups)],
				MessageID:        fmt.Sprintf("<part%d.file%d@news>", j, i),
			})
			offset += 700000
		}
		file.Size = offset
		nzb.Files = append(nzb.Files, file)
	}
	return nzb
}

func TestDecodeFileV2MatchesFullDecode(t *testing.T) {
	t.Parallel()
	codec := testCodec(t)
	data := codec.encodeNZBV2(buildCodecNZB())
	full, err := codec.decodeNZBV2(data)
	if err != nil {
		t.Fatalf("full decode: %v", err)
	}

	for i := range full.Files {
		want := full.Files[i]
		got, decodeFileV2Err := codec.decodeFileV2(data, want.Name)
		if want.IsDeleted {
			if !errors.Is(decodeFileV2Err, errFileNotFound) {
				t.Fatalf("codec.decodeFileV2(%q) on a deleted file: %v", want.Name, decodeFileV2Err)
			}
			continue
		}
		if decodeFileV2Err != nil {
			t.Fatalf("codec.decodeFileV2(%q): %v", want.Name, decodeFileV2Err)
		}
		if !reflect.DeepEqual(*got, want) {
			t.Fatalf("codec.decodeFileV2(%q) mismatch:\n got %+v\nwant %+v", want.Name, *got, want)
		}
	}
}

func TestDecodeFileV2MissingFile(t *testing.T) {
	t.Parallel()
	codec := testCodec(t)
	data := codec.encodeNZBV2(buildCodecNZB())
	if _, err := codec.decodeFileV2(data, "not-here.mkv"); !errors.Is(err, errFileNotFound) {
		t.Fatalf("decodeFileV2 error = %v, want errFileNotFound", err)
	}
}

// The point of the single-file path: its message ids must be owned copies, not
// views into the decompressed buffer that the full decode aliases.
func TestDecodeFileV2CopiesMessageIDs(t *testing.T) {
	t.Parallel()
	codec := testCodec(t)
	data := codec.encodeNZBV2(buildCodecNZB())
	file, err := codec.decodeFileV2(data, "file-4.mkv")
	if err != nil {
		t.Fatalf("decodeFileV2: %v", err)
	}
	if file == nil || len(file.Segments) == 0 {
		t.Fatal("expected segments")
	}
	_, _, mc, err := splitRegions(data)
	if err != nil {
		t.Fatalf("splitRegions: %v", err)
	}
	msgIDs, err := codec.dec.DecodeAll(mc, nil)
	if err != nil {
		t.Fatalf("decompress msg ids: %v", err)
	}
	lo := uintptr(unsafe.Pointer(&msgIDs[0]))
	hi := lo + uintptr(len(msgIDs))
	for _, segment := range file.Segments {
		p := uintptr(unsafe.Pointer(unsafe.StringData(segment.MessageID)))
		if p >= lo && p < hi {
			t.Fatalf("message id %q aliases the decompressed buffer", segment.MessageID)
		}
	}
}

func testCodec(t testing.TB) *nzbCodec {
	t.Helper()
	codec, err := newNZBCodec()
	if err != nil {
		t.Fatal(err)
	}
	return codec
}
