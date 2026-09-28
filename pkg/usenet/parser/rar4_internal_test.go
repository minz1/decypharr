package parser

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func rar4Block(kind byte, flags uint16, body []byte) []byte {
	block := make([]byte, rar4BaseHeaderSize, rar4BaseHeaderSize+len(body))
	block[2] = kind
	binary.LittleEndian.PutUint16(block[3:], flags)
	binary.LittleEndian.PutUint16(block[5:], uint16(rar4BaseHeaderSize+len(body)))
	return append(block, body...)
}

// rar4FileBody lays out FILE_HEAD/NEWSUB_HEAD fields after the base header.
func rar4FileBody(name string, packed, unpacked uint32, method byte) []byte {
	body := make([]byte, rar4MinFileHeaderData, rar4MinFileHeaderData+len(name))
	binary.LittleEndian.PutUint32(body[0:], packed)
	binary.LittleEndian.PutUint32(body[4:], unpacked)
	body[8] = 2 // host OS
	body[17] = 29
	body[18] = method
	binary.LittleEndian.PutUint16(body[19:], uint16(len(name)))
	binary.LittleEndian.PutUint32(body[21:], 0x20) // archive attribute
	return append(body, name...)
}

func TestRAR4SnippetParserReadsLongBlockFileHeader(t *testing.T) {
	t.Parallel()
	archive := rar4Block(RAR4HeaderTypeArchive, 0, make([]byte, 6))
	file := rar4Block(RAR4HeaderTypeFile, RAR4HeaderFlagLongBlock,
		rar4FileBody("movie.mkv", 100, 100, RAR4CompressionMethodStore))
	data := append([]byte(RAR4Signature), archive...)
	data = append(data, file...)
	data = append(data, make([]byte, 100)...)

	files, err := (&RARParser{}).parseRAR4Headers(data, 0, "a.rar")
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%d err=%v", len(files), err)
	}
	got := files[0]
	wantOffset := int64(len(RAR4Signature) + len(archive) + len(file))
	if got.Name != "movie.mkv" || !got.IsStored || got.PackedSize != 100 || got.UncompressedSize != 100 ||
		got.DataOffset != wantOffset {
		t.Fatalf("entry = %+v, want stored movie.mkv 100/100 at %d", got, wantOffset)
	}
}

func TestRAR4StreamSkipsServiceHeaderData(t *testing.T) {
	t.Parallel()
	comment := []byte("release comment!")
	service := rar4Block(RAR4HeaderTypeService, RAR4HeaderFlagLongBlock,
		rar4FileBody("CMT", uint32(len(comment)), uint32(len(comment)), RAR4CompressionMethodStore))
	file := rar4Block(RAR4HeaderTypeFile, RAR4HeaderFlagLongBlock,
		rar4FileBody("movie.mkv", 64, 64, RAR4CompressionMethodStore))
	var data []byte
	data = append(data, rar4Block(RAR4HeaderTypeArchive, 0, make([]byte, 6))...)
	data = append(data, service...)
	data = append(data, comment...)
	data = append(data, file...)
	data = append(data, bytes.Repeat([]byte{0xAB}, 64)...)
	data = append(data, rar4Block(RAR4HeaderTypeEnd, 0, nil)...)

	stream := &rarReader{ctx: t.Context(), currentSegmentData: data}
	files, err := (&RARParser{}).parseRAR4Stream(stream, 0, "a.rar", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "movie.mkv" {
		t.Fatalf("files = %+v, want movie.mkv after the comment block", files)
	}
	wantOffset := int64(len(data) - 64 - rar4BaseHeaderSize)
	if files[0].DataOffset != wantOffset {
		t.Fatalf("data offset = %d, want %d", files[0].DataOffset, wantOffset)
	}
}

func TestRAR5HeaderRejectsDataSizeBeyondInt64(t *testing.T) {
	t.Parallel()
	var content []byte
	for _, value := range []uint64{RAR5HeaderTypeFile, RAR5HeaderFlagDataArea, 1 << 63} {
		content = binary.AppendUvarint(content, value)
	}
	raw := binary.AppendUvarint(make([]byte, 4), uint64(len(content)))
	raw = append(raw, content...)
	parser := &RARParser{}
	if _, _, size, err := parser.readRAR5Header(bytes.NewReader(raw)); err == nil {
		t.Fatalf("data size %d accepted", size)
	}
	stream := &rarReader{ctx: t.Context(), currentSegmentData: raw}
	if _, _, size, err := parser.readRAR5HeaderFromStream(stream); err == nil {
		t.Fatalf("stream data size %d accepted", size)
	}
}

func TestRAR4DataSizeRejectsOverflow(t *testing.T) {
	t.Parallel()
	body := rar4FileBody("x", 1, 1, RAR4CompressionMethodStore)
	body = append(body[:rar4HighPackSizeOffset], append([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 0}, 'x')...)
	header := &rar4Header{
		Type:  RAR4HeaderTypeFile,
		Flags: RAR4HeaderFlagLongBlock | RAR4FileFlagHighSize,
		Data:  body,
	}
	if size, ok := rar4DataSize(header); ok {
		t.Fatalf("size = %d accepted for a high word beyond int64", size)
	}
	if entry := (&RARParser{}).parseRAR4FileHeader(header, 0, "a.rar", 0); entry != nil {
		t.Fatalf("entry = %+v, want nil for overflowing sizes", entry)
	}
}
