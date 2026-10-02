package rar

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

// block builds a RAR3 block header of the given size; extra is written right
// after the base header and the rest is filled with 0xFF.
func block(headType byte, flags uint16, size int, extra []byte) []byte {
	b := bytes.Repeat([]byte{0xFF}, max(size, baseHeaderSize))
	b[0], b[1], b[2] = 0, 0, headType
	binary.LittleEndian.PutUint16(b[3:5], flags)
	binary.LittleEndian.PutUint16(b[5:7], uint16(size))
	copy(b[baseHeaderSize:], extra)
	return b
}

func fileBlock(name string) []byte {
	h := make([]byte, fileHeaderSize+len(name))
	h[2] = blockFile
	binary.LittleEndian.PutUint16(h[3:5], flagHasData)
	binary.LittleEndian.PutUint16(h[5:7], uint16(len(h)))
	binary.LittleEndian.PutUint32(h[7:11], 3)  // pack size
	binary.LittleEndian.PutUint32(h[11:15], 3) // unpacked size
	binary.LittleEndian.PutUint16(h[26:28], uint16(len(name)))
	copy(h[fileHeaderSize:], name)
	return append(h, "abc"...)
}

func serveArchive(t *testing.T, parts ...[]byte) string {
	t.Helper()
	archive := append([]byte(rar3Marker), block(blockHeader, 0, 13, nil)...)
	for _, p := range parts {
		archive = append(archive, p...)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "a.rar", time.Time{}, bytes.NewReader(archive))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestReadFilesRejectsZeroSizeBlock(t *testing.T) {
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	reader, err := NewReader(ctx, serveArchive(t, block(0x7A, 0, 0, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reader.GetFiles(ctx); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("GetFiles() error = %v, want ErrInvalidFormat", err)
	}
}

func TestReadFilesSkipsLongBlockByAddSize(t *testing.T) {
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	// A 20-byte sub-block whose ADD_SIZE (5) follows the base header; its
	// last four header bytes are 0xFF and must not be read as the data size.
	sub := append(block(0x7A, flagHasData, 20, []byte{5, 0, 0, 0}), "xxxxx"...)
	url := serveArchive(t, sub, fileBlock("movie.mkv"), block(blockEnd, 0, baseHeaderSize, nil))
	reader, err := NewReader(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	files, err := reader.GetFiles(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name() != "movie.mkv" {
		t.Fatalf("GetFiles() = %v, want movie.mkv", files)
	}
}

func TestDecodeUnicode(t *testing.T) {
	t.Parallel()
	// Flags 0b_10_01_00: ASCII "a", low byte 0xE9, high byte 0 + 0x41, ASCII "b"; then the rest.
	got := decodeUnicode("abc", []byte{0b100100, 0xE9, 0x41})
	if got != "aéAbc" {
		t.Fatalf("decodeUnicode() = %q, want %q", got, "aéAbc")
	}
}
