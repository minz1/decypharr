// Source: https://github.com/eliasbenb/RARAR.py
// Note that this code only translates the original Python for RAR3 (not RAR5) support.

package rar

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/retry"
)

const (
	httpChunkSize  = 32768
	firstChunkSize = 8192
	maxSearchSize  = 1 << 20 // 1MB

	// rar3Marker is the RAR 1.5-4.x signature.
	rar3Marker = "Rar!\x1a\x07\x00"

	// Block types.
	blockFile   = 0x74
	blockHeader = 0x73
	blockEnd    = 0x7B

	// Header flags.
	flagDirectory      = 0xE0
	flagHasHighSize    = 0x100
	flagHasUnicodeName = 0x200
	flagHasData        = 0x8000 // LONG_BLOCK: ADD_SIZE follows the base header

	// Base block header: HEAD_CRC(2) HEAD_TYPE(1) HEAD_FLAGS(2) HEAD_SIZE(2).
	baseHeaderSize = 7
	// addSizeLen is the length of ADD_SIZE, stored right after the base header.
	addSizeLen = 4
	// fileHeaderSize is the fixed part of a file header before HIGH_*_SIZE.
	fileHeaderSize = 32
	// highSizeLen is HIGH_PACK_SIZE(4) + HIGH_UNP_SIZE(4).
	highSizeLen = 8
	// highSizeShift places HIGH_*_SIZE above the low 32 bits.
	highSizeShift = 32
	// blockReadAttempts bounds retries of one header read.
	blockReadAttempts = 4
)

// Error definitions.
var (
	ErrMarkerNotFound               = errors.New("RAR marker not found within search limit")
	ErrInvalidFormat                = errors.New("invalid RAR format")
	ErrNetworkError                 = errors.New("network error")
	ErrRangeRequestsNotSupported    = errors.New("server does not support range requests")
	ErrCompressionNotSupported      = errors.New("compression method not supported")
	ErrDirectoryExtractNotSupported = errors.New("directory extract not supported")

	errShortRead = errors.New("short read")
)

// Name returns the base filename of the file.
func (f *File) Name() string {
	if i := strings.LastIndexAny(f.Path, "\\/"); i >= 0 {
		return f.Path[i+1:]
	}
	return f.Path
}

// ByteRange returns the inclusive byte range of the file's stored data.
func (f *File) ByteRange() *[2]int64 {
	return &[2]int64{f.DataOffset, f.DataOffset + f.CompressedSize - 1}
}

// NewReader opens the RAR3 archive at url and validates its archive header.
// All requests made while opening are bound to ctx.
func NewReader(ctx context.Context, url string) (*Reader, error) {
	file, err := NewHTTPFile(ctx, url)
	if err != nil {
		return nil, err
	}

	reader := &Reader{
		File:      file,
		ChunkSize: httpChunkSize,
		Files:     make([]*File, 0),
	}

	marker, err := reader.findMarker(ctx)
	if err != nil {
		return nil, err
	}
	reader.Marker = marker
	pos := reader.Marker + int64(len(rar3Marker)) // Skip marker block

	headerData, err := reader.readBytes(ctx, pos, baseHeaderSize)
	if err != nil {
		return nil, err
	}
	if len(headerData) < baseHeaderSize || headerData[2] != blockHeader {
		return nil, ErrInvalidFormat
	}
	headSize := int64(binary.LittleEndian.Uint16(headerData[5:7]))

	// Store the position after the archive header
	reader.HeaderEndPos = pos + headSize
	return reader, nil
}

// readBytes reads up to length bytes at start; a read past EOF is short.
func (r *Reader) readBytes(ctx context.Context, start int64, length int) ([]byte, error) {
	if length <= 0 {
		return []byte{}, nil
	}
	data := make([]byte, length)
	n, err := r.File.ReadAtContext(ctx, data, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return data[:n], nil
}

// readExact reads exactly length bytes at start, retrying transient failures.
func (r *Reader) readExact(ctx context.Context, start int64, length int) ([]byte, error) {
	var data []byte
	err := retry.Do(
		func() error {
			var readErr error
			data, readErr = r.readBytes(ctx, start, length)
			if readErr != nil {
				if !errors.Is(readErr, ErrNetworkError) || ctx.Err() != nil {
					return retry.Unrecoverable(readErr)
				}
				return readErr
			}
			if len(data) < length {
				// A short read is end of file, not a transient failure.
				return retry.Unrecoverable(
					fmt.Errorf("%w at %d: %d of %d bytes", errShortRead, start, len(data), length),
				)
			}
			return nil
		},
		retry.Attempts(blockReadAttempts),
		retry.Delay(config.DefaultRetryDelay),
		retry.MaxDelay(config.DefaultRetryDelayMax),
		retry.DelayType(retry.BackOffDelay),
	)
	return data, err
}

// findMarker finds the RAR marker in the file.
func (r *Reader) findMarker(ctx context.Context) (int64, error) {
	marker := []byte(rar3Marker)
	chunk, err := r.readBytes(ctx, 0, firstChunkSize)
	if err != nil {
		return 0, err
	}
	if markerPos := bytes.Index(chunk, marker); markerPos != -1 {
		return int64(markerPos), nil
	}

	// If not found, continue searching
	position := int64(firstChunkSize - len(marker) + 1)
	for position < maxSearchSize {
		chunkSize := min(r.ChunkSize, int(maxSearchSize-position))
		next, readErr := r.readBytes(ctx, position, chunkSize)
		if readErr != nil || len(next) == 0 {
			break
		}
		if markerPos := bytes.Index(next, marker); markerPos != -1 {
			return position + int64(markerPos), nil
		}
		// Move forward by chunk size minus the marker length
		position += int64(max(1, len(next)-len(marker)+1))
	}
	return 0, ErrMarkerNotFound
}

// RAR3 Unicode name encoding: each flag byte holds four 2-bit operations.
const (
	opASCII      = 0 // copy the next ASCII byte
	opLowByte    = 1 // next data byte, high byte 0
	opWithHigh   = 2 // next data byte with the current high byte
	opMask       = 0x03
	opBits       = 2
	opsPerByte   = 4
	flagExtended = 0x80
	byteBits     = 8
)

// unicodeFlags reads one RAR3 Unicode flag group at data[pos] and returns the
// 2-bit operations, how many characters they control, and the next position.
func unicodeFlags(data []byte, pos int) (uint, int, int) {
	flags := data[pos]
	pos++
	if flags&flagExtended == 0 {
		return uint(flags), opsPerByte, pos
	}
	// Extended flag: continuation bytes extend the group.
	bits := uint(flags)
	count := 1
	for (bits&(flagExtended>>count) != 0) && pos < len(data) {
		bits = ((bits & ((flagExtended >> count) - 1)) << byteBits) | uint(data[pos])
		pos++
		count++
	}
	return bits, count * opsPerByte, pos
}

// unicodeDecoder holds the state of one RAR3 Unicode name decode.
type unicodeDecoder struct {
	ascii    string
	data     []byte
	asciiPos int
	dataPos  int
	high     byte
	out      []rune
}

func (d *unicodeDecoder) exhausted() bool {
	return d.asciiPos >= len(d.ascii) && d.dataPos >= len(d.data)
}

// apply executes one 2-bit operation.
func (d *unicodeDecoder) apply(op uint) {
	if op == opASCII {
		if d.asciiPos < len(d.ascii) {
			d.out = append(d.out, rune(d.ascii[d.asciiPos]))
			d.asciiPos++
		}
		return
	}
	if d.dataPos >= len(d.data) {
		return
	}
	b := d.data[d.dataPos]
	d.dataPos++
	switch op {
	case opLowByte:
		d.out = append(d.out, rune(b))
	case opWithHigh:
		d.out = append(d.out, rune(d.high)<<byteBits|rune(b))
	default: // set a new high byte
		d.high = b
	}
}

// decodeUnicode decodes RAR3 Unicode encoding.
func decodeUnicode(asciiStr string, unicodeData []byte) string {
	if len(unicodeData) == 0 {
		return asciiStr
	}
	d := &unicodeDecoder{ascii: asciiStr, data: unicodeData}
	for d.dataPos < len(d.data) {
		ops, count, next := unicodeFlags(d.data, d.dataPos)
		d.dataPos = next
		for i := 0; i < count && !d.exhausted(); i++ {
			d.apply((ops >> (i * opBits)) & opMask)
		}
	}
	// Append any remaining ASCII characters
	for ; d.asciiPos < len(d.ascii); d.asciiPos++ {
		d.out = append(d.out, rune(d.ascii[d.asciiPos]))
	}
	return string(d.out)
}

// readFiles reads all file entries in the archive.
func (r *Reader) readFiles(ctx context.Context) error {
	// NewReader already validated the archive header and stored where it ends.
	pos := r.HeaderEndPos

	// Process all blocks until blockEnd or EOF.
	for {
		headerData, err := r.readExact(ctx, pos, baseHeaderSize)
		if errors.Is(err, errShortRead) {
			break // end of data without an end block
		}
		if err != nil {
			return fmt.Errorf("read block header at offset %d: %w", pos, err)
		}
		headType := headerData[2]
		headFlags := int(binary.LittleEndian.Uint16(headerData[3:5]))
		headSize := int(binary.LittleEndian.Uint16(headerData[5:7]))
		if headType == blockEnd {
			break
		}
		// Every block covers at least its own base header; anything smaller
		// would never advance pos and loop forever on a malformed archive.
		if headSize < baseHeaderSize {
			return fmt.Errorf("%w: block at offset %d has header size %d", ErrInvalidFormat, pos, headSize)
		}
		if pos, err = r.nextBlock(ctx, pos, headType, headFlags, headSize); err != nil {
			return err
		}
	}
	return nil
}

// nextBlock consumes the block at pos and returns the offset of the next one.
func (r *Reader) nextBlock(ctx context.Context, pos int64, headType byte, headFlags, headSize int) (int64, error) {
	if headType == blockFile {
		header, err := r.readExact(ctx, pos, headSize)
		if err != nil {
			return 0, fmt.Errorf("failed to read complete file header after retries: %w", err)
		}
		if fileInfo, parseErr := r.parseFileHeader(header, pos); parseErr == nil {
			r.Files = append(r.Files, fileInfo)
			return fileInfo.NextOffset, nil
		}
		return pos + int64(headSize), nil
	}

	// Skip a non-file block and, for long blocks, its data.
	next := pos + int64(headSize)
	if headFlags&flagHasData != 0 {
		sizeData, err := r.readExact(ctx, pos+baseHeaderSize, addSizeLen)
		if err != nil {
			return 0, fmt.Errorf("failed to read data size after retries: %w", err)
		}
		next += int64(binary.LittleEndian.Uint32(sizeData))
	}
	return next, nil
}

// fileName decodes a RAR3 file name field.
func fileName(nameBytes []byte, headFlags int) string {
	if headFlags&flagHasUnicodeName == 0 {
		return string(nameBytes)
	}
	asciiPart, unicodePart, ok := bytes.Cut(nameBytes, []byte{0})
	if !ok || utf8.Valid(asciiPart) {
		return string(asciiPart)
	}
	return decodeUnicode(string(asciiPart), unicodePart)
}

// parseFileHeader parses a file header and returns file info.
func (r *Reader) parseFileHeader(headerData []byte, position int64) (*File, error) {
	if len(headerData) < baseHeaderSize {
		return nil, fmt.Errorf("header data too short")
	}

	headType := headerData[2]
	headFlags := int(binary.LittleEndian.Uint16(headerData[3:5]))
	headSize := int(binary.LittleEndian.Uint16(headerData[5:7]))

	if headType != blockFile {
		return nil, fmt.Errorf("not a file block")
	}
	if len(headerData) < fileHeaderSize {
		return nil, fmt.Errorf("file header too short")
	}

	packSize := int64(binary.LittleEndian.Uint32(headerData[7:11]))
	unpackSize := int64(binary.LittleEndian.Uint32(headerData[11:15]))
	fileCRC := binary.LittleEndian.Uint32(headerData[16:20])
	method := headerData[25]
	nameSize := int(binary.LittleEndian.Uint16(headerData[26:28]))

	offset := fileHeaderSize
	if headFlags&flagHasHighSize != 0 {
		if offset+highSizeLen <= len(headerData) {
			packSize += int64(binary.LittleEndian.Uint32(headerData[offset:offset+4])) << highSizeShift
			unpackSize += int64(binary.LittleEndian.Uint32(headerData[offset+4:offset+highSizeLen])) << highSizeShift
		}
		offset += highSizeLen
	}

	name := fmt.Sprintf("UnknownFile%d", len(r.Files))
	if offset+nameSize <= len(headerData) {
		name = fileName(headerData[offset:offset+nameSize], headFlags)
	}

	isDirectory := (headFlags & flagDirectory) == flagDirectory

	// Calculate data offsets
	dataOffset := position + int64(headSize)
	nextOffset := dataOffset
	if !isDirectory && headFlags&flagHasData != 0 {
		nextOffset += packSize
	}

	return &File{
		Path:           name,
		Size:           unpackSize,
		CompressedSize: packSize,
		Method:         method,
		CRC:            fileCRC,
		IsDirectory:    isDirectory,
		DataOffset:     dataOffset,
		NextOffset:     nextOffset,
	}, nil
}

// GetFiles returns all files in the archive, reading the headers on first use.
func (r *Reader) GetFiles(ctx context.Context) ([]*File, error) {
	if len(r.Files) == 0 {
		if err := r.readFiles(ctx); err != nil {
			return nil, err
		}
	}
	return r.Files, nil
}
