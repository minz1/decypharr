package parser

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/javi11/sevenzip"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// rar4Volume builds a RAR4 volume holding one stored part of movie.mkv.
func rar4Volume(part []byte, unpacked uint32) []byte {
	volume := append([]byte(RAR4Signature), rar4Block(RAR4HeaderTypeArchive, 0, make([]byte, 6))...)
	volume = append(volume, rar4Block(RAR4HeaderTypeFile, RAR4HeaderFlagLongBlock,
		rar4FileBody("movie.mkv", uint32(len(part)), unpacked))...)
	return append(volume, part...)
}

// A RAR file spanning volumes inside a 7z is mapped across every volume, in
// logical order, though 7z stores .r00 before .rar.
func TestEmbeddedMultiVolumeRARSpansAllVolumes(t *testing.T) {
	t.Parallel()
	first := bytes.Repeat([]byte{'A'}, 100)
	second := bytes.Repeat([]byte{'B'}, 50)
	rar := rar4Volume(first, 150)
	r00 := rar4Volume(second, 150)
	blob := append(bytes.Clone(r00), rar...)
	rarFiles := []sevenzip.FileInfo{
		{Name: "movie.r00", Offset: 0, Size: uint64(len(r00))},
		{Name: "movie.rar", Offset: int64(len(r00)), Size: uint64(len(rar))},
	}

	const segmentSize = 64
	var source []storage.NZBSegment
	for off := 0; off < len(blob); off += segmentSize {
		source = append(source, storage.NZBSegment{
			MessageID: strconv.Itoa(off / segmentSize),
			Bytes:     int64(min(segmentSize, len(blob)-off)),
		})
	}
	layout, err := newSegmentLayout(source)
	if err != nil {
		t.Fatal(err)
	}

	p := NewSevenZParser(nil, 1, zerolog.Nop())
	files, err := p.processRARFilesFromPositions(rarFiles, &FileGroup{}, bytes.NewReader(blob), layout, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	var content []byte
	for _, segment := range files[0].Segments {
		index, _ := strconv.Atoi(segment.MessageID)
		start := index*segmentSize + int(segment.SegmentDataStart)
		content = append(content, blob[start:start+int(segment.Bytes)]...)
	}
	if want := string(first) + string(second); string(content) != want {
		t.Fatalf("mapped %d bytes %q..., want %d bytes: the .rar part, then the .r00 part",
			len(content), summarize(content), len(want))
	}
}

func summarize(b []byte) string {
	var s strings.Builder
	for i := 0; i < len(b); {
		j := i
		for j < len(b) && b[j] == b[i] {
			j++
		}
		fmt.Fprintf(&s, "%dx%c ", j-i, b[i])
		i = j
	}
	return s.String()
}
