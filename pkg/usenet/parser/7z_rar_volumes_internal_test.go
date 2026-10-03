package parser

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

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

// countingReaderAt records which offsets were read and how many reads ran at
// once.
type countingReaderAt struct {
	data     []byte
	mu       sync.Mutex
	inFlight int
	peak     int
	offsets  []int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.mu.Lock()
	c.inFlight++
	c.peak = max(c.peak, c.inFlight)
	c.offsets = append(c.offsets, off)
	c.mu.Unlock()
	time.Sleep(time.Millisecond) // an article fetch
	c.mu.Lock()
	c.inFlight--
	c.mu.Unlock()
	if off >= int64(len(c.data)) {
		return 0, io.EOF
	}
	n := copy(p, c.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// rar4VolumeSet lays out volumes .rar, .r00, ... of one stored file; every
// volume ends with an end block, flagged "next volume" on all but the last.
func rar4VolumeSet(count int) ([]sevenzip.FileInfo, []byte) {
	var blob []byte
	var infos []sevenzip.FileInfo
	for i := range count {
		volume := rar4Volume(bytes.Repeat([]byte{byte('A' + i)}, 10), uint32(10*count))
		var endFlags uint16
		if i < count-1 {
			endFlags = rar4EndFlagNextVolume
		}
		volume = append(volume, rar4Block(RAR4HeaderTypeEnd, endFlags, nil)...)
		name := "movie.rar"
		if i > 0 {
			name = fmt.Sprintf("movie.r%02d", i-1)
		}
		infos = append(infos, sevenzip.FileInfo{Name: name, Offset: int64(len(blob)), Size: uint64(len(volume))})
		blob = append(blob, volume...)
	}
	return infos, blob
}

// Volume heads are fetched in parallel, up to the parser's concurrency; the
// scan used to fetch them one by one.
func TestEmbeddedRARHeadsAreFetchedConcurrently(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		infos, blob := rar4VolumeSet(12)
		reader := &countingReaderAt{data: blob}
		p := NewSevenZParser(nil, 4, zerolog.Nop())
		files := p.scanEmbeddedRARHeaders(infos, reader, RARVersion4, "")
		if len(files) != 12 {
			t.Fatalf("parsed %d volume parts, want 12", len(files))
		}
		if reader.peak != 4 {
			t.Fatalf("at most %d heads were fetched at once, want 4", reader.peak)
		}
	})
}

// The scan stops at the volume whose end-of-archive header says no volume
// follows.
func TestEmbeddedRARScanStopsAtLastVolume(t *testing.T) {
	t.Parallel()
	infos, blob := rar4VolumeSet(2)
	// A stray third volume after the archive's last one.
	stray, _ := rar4VolumeSet(1)
	stray[0].Name, stray[0].Offset = "movie.r01", int64(len(blob))
	infos = append(infos, stray[0])
	reader := &countingReaderAt{data: append(blob, blob[:stray[0].Size]...)}

	p := NewSevenZParser(nil, 1, zerolog.Nop())
	p.scanEmbeddedRARHeaders(infos, reader, RARVersion4, "")
	for _, off := range reader.offsets {
		if off >= stray[0].Offset {
			t.Fatalf("read the head at %d, past the archive's last volume", off)
		}
	}
}
