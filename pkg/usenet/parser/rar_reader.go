package parser

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/sirrobot01/decypharr/internal/crypto"
	"github.com/sirrobot01/decypharr/pkg/usenet/types"
)

// rarReader provides a continuous stream across RAR volumes
// It can efficiently skip large data sections without downloading them.
type rarReader struct {
	ctx      context.Context
	source   ArticleSource
	volumes  []*types.Volume
	position int64 // Current absolute position in the archive

	currentVolumeIndex   int
	currentSegmentIndex  int
	currentSegmentData   []byte
	currentSegmentOffset int // Offset within current segment data
}

func newRarReader(ctx context.Context, source ArticleSource, volumes []*types.Volume) *rarReader {
	return &rarReader{
		ctx:                 ctx,
		source:              source,
		volumes:             volumes,
		position:            0,
		currentVolumeIndex:  0,
		currentSegmentIndex: 0,
	}
}

// Read implements [io.Reader].
func (r *rarReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	totalRead := 0

	for totalRead < len(p) {
		// Ensure we have current segment data
		if r.currentSegmentData == nil || r.currentSegmentOffset >= len(r.currentSegmentData) {
			if err := r.loadNextSegment(); err != nil {
				if totalRead > 0 {
					return totalRead, nil
				}
				return 0, err
			}
		}

		// Copy from current segment
		n := copy(p[totalRead:], r.currentSegmentData[r.currentSegmentOffset:])
		r.currentSegmentOffset += n
		r.position += int64(n)
		totalRead += n
	}

	return totalRead, nil
}

// Skip efficiently skips n bytes without downloading unnecessary data.
func (r *rarReader) Skip(n int64) error {
	if n <= 0 {
		return nil
	}

	// Fast path: skip within current segment if possible
	if r.currentSegmentData != nil {
		remaining := len(r.currentSegmentData) - r.currentSegmentOffset
		if int64(remaining) >= n {
			r.currentSegmentOffset += int(n)
			r.position += n
			return nil
		}
		// Skip rest of current segment
		r.position += int64(remaining)
		n -= int64(remaining)
		r.currentSegmentData = nil
	}

	// Skip entire segments without downloading
	for n > 0 {
		if r.currentVolumeIndex >= len(r.volumes) {
			return io.EOF
		}

		volume := r.volumes[r.currentVolumeIndex]
		if r.currentSegmentIndex >= len(volume.Segments) {
			// Move to next volume
			r.currentVolumeIndex++
			r.currentSegmentIndex = 0
			continue
		}

		segment := volume.Segments[r.currentSegmentIndex]
		segmentSize := segment.Bytes

		if n >= segmentSize {
			// Skip entire segment
			r.currentSegmentIndex++
			r.position += segmentSize
			n -= segmentSize
		} else {
			// Need to load this segment and skip within it
			if err := r.loadNextSegment(); err != nil {
				return err
			}
			r.currentSegmentOffset = int(n)
			r.position += n
			n = 0
		}
	}

	return nil
}

// loadNextSegment loads the next segment's data.
func (r *rarReader) loadNextSegment() error {
	for {
		if r.currentVolumeIndex >= len(r.volumes) {
			return io.EOF
		}

		volume := r.volumes[r.currentVolumeIndex]
		if r.currentSegmentIndex >= len(volume.Segments) {
			// Move to next volume
			r.currentVolumeIndex++
			r.currentSegmentIndex = 0
			continue
		}

		segment := volume.Segments[r.currentSegmentIndex]

		data, err := fetchSegmentData(r.ctx, r.source, segment)
		if err != nil {
			return fmt.Errorf("failed to fetch segment: %w", err)
		}

		r.currentSegmentData = data
		r.currentSegmentOffset = 0
		r.currentSegmentIndex++
		return nil
	}
}

// Position returns the current position in the stream.
func (r *rarReader) Position() int64 {
	return r.position
}

// AbsoluteToVolumeOffset converts an absolute position in the stream to (volumeIndex, offsetWithinVolume).
func (r *rarReader) AbsoluteToVolumeOffset(absolutePos int64) (int, int64) {
	currentPos := int64(0)

	for volIdx, volume := range r.volumes {
		// Calculate total size of this volume (sum of all segment sizes)
		volumeSize := int64(0)
		for _, segment := range volume.Segments {
			volumeSize += segment.Bytes
		}

		// Check if the position falls within this volume
		if absolutePos < currentPos+volumeSize {
			// Position is in this volume
			offsetInVol := absolutePos - currentPos
			return volIdx, offsetInVol
		}

		currentPos += volumeSize
	}

	// If we get here, position is beyond all volumes
	// Return last volume and offset
	if len(r.volumes) > 0 {
		return len(r.volumes) - 1, absolutePos
	}
	return 0, absolutePos
}

// parseRAR5StreamResult contains the result of parsing a RAR5 stream.
type parseRAR5StreamResult struct {
	Files             []*RARFileEntry
	IsHeaderEncrypted bool
	EncryptionKey     []byte // AES key for file data decryption (if encrypted)
	EncryptionIV      []byte // AES IV for file data decryption (if encrypted)
}

// rar5Volume identifies the volume whose entries a stream parse emits.
type rar5Volume struct {
	index    int
	name     string
	password string
}

// parseRAR5Stream parses RAR 5.0 headers from a stream reader
// This properly tracks offsets by reading headers sequentially and skipping data
// If password is provided and headers are encrypted, it will decrypt them.
// A header that cannot be read (truncated or corrupt data after the last
// parseable header) ends the scan rather than failing the volume.
func (p *RARParser) parseRAR5Stream(
	stream *rarReader,
	volumeIndex int,
	volumeName string,
	password string,
) (*parseRAR5StreamResult, error) {
	result := &parseRAR5StreamResult{
		Files: make([]*RARFileEntry, 0),
	}
	vol := rar5Volume{index: volumeIndex, name: volumeName, password: password}

	// Stream position is already at 8 (after signature)
	for {
		headerStartPos := stream.Position()
		header, headerSize, dataSize, ok := p.nextRAR5Header(stream)
		if !ok {
			return result, nil
		}

		// An encryption header means every following header is encrypted.
		if header.Type == RAR5HeaderTypeEncrypt {
			result.IsHeaderEncrypted = true
			return result, p.parseEncryptedRAR5Headers(stream, header, vol, result)
		}

		// Data starts immediately after the header. Use the volume passed in,
		// not the stream's index: each stream only contains one volume.
		if header.Type == RAR5HeaderTypeFile {
			_, offsetInVol := stream.AbsoluteToVolumeOffset(headerStartPos + int64(headerSize))
			p.appendRAR5File(result, header, vol, offsetInVol, dataSize)
		}

		// Skip the data section to get to the next header
		if dataSize > 0 {
			if skipErr := stream.Skip(dataSize); skipErr != nil {
				if errors.Is(skipErr, io.EOF) {
					return result, nil
				}
				return nil, fmt.Errorf("failed to skip data section: %w", skipErr)
			}
		}

		if header.Type == RAR5HeaderTypeEndOfArc {
			return result, nil
		}
	}
}

// parseEncryptedRAR5Headers reads the encrypted headers that follow an
// encryption header. Each one is a 16-byte IV plus AES-CBC data aligned to
// the block size. Without a usable password (or once the data runs out) the
// scan simply ends; only a failed password check is an error.
func (p *RARParser) parseEncryptedRAR5Headers(
	stream *rarReader,
	header *rar5HeaderData,
	vol rar5Volume,
	result *parseRAR5StreamResult,
) error {
	encryption, ok := parseRAR5EncryptionHeader(header.Data)
	if !ok || vol.password == "" {
		return nil
	}
	keys := crypto.DeriveKeys([]byte(vol.password), encryption.Salt, encryption.KdfCount)
	if encryption.HasPwCheck && !crypto.VerifyPassword(keys, encryption.PwCheck) {
		return crypto.ErrBadPassword
	}
	result.EncryptionKey = keys.Key

	for {
		iv := make([]byte, crypto.BlockSize)
		if !readFullOK(stream, iv) {
			return nil
		}
		result.EncryptionIV = iv

		encHeader, _, encDataSize, ok := p.nextEncryptedRAR5Header(stream, keys.Key, iv)
		if !ok {
			return nil
		}
		if encHeader.Type == RAR5HeaderTypeFile {
			// Headers being encrypted does not mean data is; the extra
			// area sets file.IsEncrypted.
			_, offsetInVol := stream.AbsoluteToVolumeOffset(stream.Position())
			p.appendRAR5File(result, encHeader, vol, offsetInVol, encDataSize)
		}
		// The data area is encrypted too, so it is padded to the block size.
		if encDataSize > 0 && !stream.trySkip(alignToBlock(encDataSize)) {
			return nil
		}
		if encHeader.Type == RAR5HeaderTypeEndOfArc {
			return nil
		}
	}
}

func (p *RARParser) appendRAR5File(
	result *parseRAR5StreamResult,
	header *rar5HeaderData,
	vol rar5Volume,
	dataOffset, dataSize int64,
) {
	file := p.parseRAR5FileHeader(
		header.Data,
		header.ExtraSize,
		vol.index,
		vol.name,
		dataOffset,
		dataSize,
		vol.password,
	)
	if file != nil {
		result.Files = append(result.Files, file)
	}
}

// nextRAR5Header reads one header; ok is false when none can be read.
func (p *RARParser) nextRAR5Header(stream *rarReader) (*rar5HeaderData, int, int64, bool) {
	header, size, dataSize, err := p.readRAR5Header(stream)
	return header, size, dataSize, err == nil
}

// nextEncryptedRAR5Header decrypts one header; ok is false when none can be read.
func (p *RARParser) nextEncryptedRAR5Header(stream *rarReader, key, iv []byte) (*rar5HeaderData, int, int64, bool) {
	header, size, dataSize, err := p.readAndDecryptRAR5Header(stream, key, iv)
	return header, size, dataSize, err == nil
}

// parseRAR5EncryptionHeader parses the archive encryption header; ok is false
// for a malformed one.
func parseRAR5EncryptionHeader(data []byte) (*crypto.EncryptionHeader, bool) {
	header, err := crypto.ParseEncryptionHeader(data)
	return header, err == nil
}

func readFullOK(r io.Reader, buf []byte) bool {
	_, err := io.ReadFull(r, buf)
	return err == nil
}

// trySkip skips n bytes and reports whether the stream had them.
func (r *rarReader) trySkip(n int64) bool {
	return r.Skip(n) == nil
}

// alignToBlock rounds n up to the AES block size.
func alignToBlock[T int | int64](n T) T {
	return (n + crypto.BlockSize - 1) / crypto.BlockSize * crypto.BlockSize
}

// readAndDecryptRAR5Header reads an encrypted RAR5 header from stream. The
// first AES block holds the CRC and the size vint, which tells how many more
// blocks the header spans. It returns the header, its encrypted (block
// aligned) size and the data-area size.
func (p *RARParser) readAndDecryptRAR5Header(stream *rarReader, key, iv []byte) (*rar5HeaderData, int, int64, error) {
	block := make([]byte, crypto.BlockSize)
	if _, err := io.ReadFull(stream, block); err != nil {
		return nil, 0, 0, err
	}
	nextIV := bytes.Clone(block) // CBC: the next block's IV is this ciphertext
	if err := crypto.DecryptBlock(block, key, iv); err != nil {
		return nil, 0, 0, err
	}

	headerSize, sizeLen := parseVIntFromBuffer(block[rar5CRCSize:])
	if sizeLen == 0 {
		return nil, 0, 0, fmt.Errorf("encrypted header size vint is truncated")
	}
	if headerSize > maxRAR5HeaderSize {
		return nil, 0, 0, fmt.Errorf("encrypted header size too large: %d", headerSize)
	}
	start := rar5CRCSize + sizeLen
	end := start + int(headerSize)
	total := alignToBlock(end)

	plain := block
	if total > crypto.BlockSize {
		rest := make([]byte, total-crypto.BlockSize)
		if _, err := io.ReadFull(stream, rest); err != nil {
			return nil, 0, 0, err
		}
		if err := crypto.DecryptBlock(rest, key, nextIV); err != nil {
			return nil, 0, 0, err
		}
		plain = slices.Concat(block, rest)
	}

	header, dataSize, err := parseRAR5HeaderBody(plain[start:end])
	if err != nil {
		return nil, 0, 0, err
	}
	return header, total, dataSize, nil
}

// readRAR5Header reads one plaintext RAR5 header. It returns the header, its
// total size (CRC + size vint + content) and the data-area size.
func (p *RARParser) readRAR5Header(r io.Reader) (*rar5HeaderData, int, int64, error) {
	var crc [rar5CRCSize]byte // not verified
	if _, err := io.ReadFull(r, crc[:]); err != nil {
		return nil, 0, 0, err
	}
	headerSize, sizeLen, err := readVIntFromReader(r)
	if err != nil {
		return nil, 0, 0, err
	}
	if headerSize > maxRAR5HeaderSize {
		return nil, 0, 0, fmt.Errorf("invalid RAR5 header size: %d (too large)", headerSize)
	}
	content := make([]byte, headerSize)
	if _, readErr := io.ReadFull(r, content); readErr != nil {
		return nil, 0, 0, readErr
	}
	header, dataSize, err := parseRAR5HeaderBody(content)
	if err != nil {
		return nil, 0, 0, err
	}
	return header, rar5CRCSize + sizeLen + len(content), dataSize, nil
}

// parseRAR5HeaderBody parses header content (everything after the CRC and the
// size vint): type, flags, the optional extra-area and data-area sizes, then
// the type-specific remainder. It returns the header and the data-area size.
func parseRAR5HeaderBody(content []byte) (*rar5HeaderData, int64, error) {
	r := bytes.NewReader(content)
	headerType, err := readVInt(r)
	if err != nil {
		return nil, 0, err
	}
	headerFlags, err := readVInt(r)
	if err != nil {
		return nil, 0, err
	}
	header := &rar5HeaderData{Type: headerType, Flags: headerFlags}
	if headerFlags&RAR5HeaderFlagExtraArea != 0 {
		if header.ExtraSize, err = readVInt(r); err != nil {
			return nil, 0, err
		}
	}
	var dataAreaSize int64
	if headerFlags&RAR5HeaderFlagDataArea != 0 {
		rawSize, sizeErr := readVInt(r)
		if sizeErr != nil {
			return nil, 0, sizeErr
		}
		if dataAreaSize, err = rar5Size(rawSize); err != nil {
			return nil, 0, err
		}
	}
	if r.Len() > 0 {
		header.Data = content[len(content)-r.Len():]
	}
	return header, dataAreaSize, nil
}

// maxRAR5Size bounds data-area and unpacked sizes so offset arithmetic and
// AES block padding on them cannot overflow int64.
const maxRAR5Size = 1 << 62

// RAR5 vint encoding: 7 payload bits per byte, high bit continues; at most
// ten bytes encode a uint64.
const (
	maxVIntLen      = 10
	vintBitsPerByte = 7
	vintPayloadMask = 0x7F
	vintContinue    = 0x80
	// maxRAR5HeaderSize bounds header allocations from corrupt sizes.
	maxRAR5HeaderSize = 64 << 10
	rar5CRCSize       = 4
)

// rar5Size converts an untrusted RAR5 size vint to int64.
func rar5Size(v uint64) (int64, error) {
	if v > maxRAR5Size {
		return 0, fmt.Errorf("RAR5 size %d out of range", v)
	}
	return int64(v), nil
}

// parseVIntFromBuffer parses a vint from a byte slice without any Read calls
// Returns (value, bytesConsumed) - bytesConsumed is 0 if buffer doesn't contain complete vint.
func parseVIntFromBuffer(buf []byte) (uint64, int) {
	var result uint64
	for i := 0; i < len(buf) && i < 10; i++ {
		b := buf[i]
		result |= uint64(b&0x7F) << (uint(i) * 7)
		if b&0x80 == 0 {
			return result, i + 1
		}
	}
	return 0, 0 // Incomplete vint
}

// readVIntFromReader reads a variable-length integer from a reader one byte
// at a time and returns the value and the number of bytes read.
func readVIntFromReader(r io.Reader) (uint64, int, error) {
	var buf [1]byte
	var result uint64
	for bytesRead := 0; bytesRead < maxVIntLen; bytesRead++ {
		n, err := r.Read(buf[:])
		if err != nil {
			return 0, bytesRead, err
		}
		if n == 0 {
			return 0, bytesRead, io.EOF
		}
		result |= uint64(buf[0]&vintPayloadMask) << (vintBitsPerByte * bytesRead)
		if buf[0]&vintContinue == 0 {
			return result, bytesRead + 1, nil
		}
	}
	return 0, maxVIntLen, fmt.Errorf("vint too large")
}

// readVInt reads a variable-length integer from [bytes.Reader] (keep for compatibility).
func readVInt(r *bytes.Reader) (uint64, error) {
	val, _, err := readVIntFromReader(r)
	return val, err
}

// parseRAR4Stream parses RAR 4.x headers from a stream reader
// This properly tracks offsets by reading headers sequentially and skipping data.
// An unreadable header (end of data or corruption) ends the scan.
func (p *RARParser) parseRAR4Stream(
	stream *rarReader,
	volumeIndex int,
	volumeName string,
	volumeSize int64,
) ([]*RARFileEntry, error) {
	var files []*RARFileEntry

	// Stream position is already past the 7-byte RAR4 signature.
	for {
		header, ok := p.nextRAR4Header(stream)
		if !ok {
			return files, nil
		}
		// Every LONG_BLOCK header (file, service/comment, recovery record)
		// is followed by a data area that must be skipped to reach the next
		// header.
		dataSkipSize, ok := rar4DataSize(header)
		if !ok {
			return files, nil
		}

		if header.Type == RAR4HeaderTypeFile {
			// Data starts immediately after the header. Use the volume passed
			// in, not the stream's index: each stream only contains one volume.
			_, offsetInVol := stream.AbsoluteToVolumeOffset(stream.Position())
			if file := p.parseRAR4StreamFile(header, volumeIndex, volumeName, offsetInVol, volumeSize); file != nil {
				files = append(files, file)
				dataSkipSize = file.PackedSize
			}
		}

		if dataSkipSize > 0 {
			if skipErr := stream.Skip(dataSkipSize); skipErr != nil {
				if errors.Is(skipErr, io.EOF) {
					return files, nil
				}
				return nil, fmt.Errorf("failed to skip RAR4 data section: %w", skipErr)
			}
		}

		if header.Type == RAR4HeaderTypeEnd {
			return files, nil
		}
	}
}

// parseRAR4StreamFile parses a file header and clamps its packed size to the
// bytes left in the volume: RAR4 headers often report the file's total packed
// size, not the part stored in this volume.
func (p *RARParser) parseRAR4StreamFile(
	header *rar4Header,
	volumeIndex int,
	volumeName string,
	offsetInVol, volumeSize int64,
) *RARFileEntry {
	file := p.parseRAR4FileHeader(header, volumeIndex, volumeName, offsetInVol)
	if file == nil {
		return nil
	}
	remainingInVolume := volumeSize - offsetInVol
	if file.PackedSize > remainingInVolume {
		file.PackedSize = remainingInVolume
		if len(file.VolumeParts) > 0 {
			file.VolumeParts[0].PackedSize = remainingInVolume
			file.VolumeParts[0].UnpackedSize = remainingInVolume // Treat as stored stream
		}
	}
	return file
}

// readRAR4HeaderFromStream reads a single RAR 4.x header from stream.
func (p *RARParser) readRAR4HeaderFromStream(stream io.Reader) (*rar4Header, error) {
	var header rar4Header

	// Read header CRC (2 bytes)
	if err := binary.Read(stream, binary.LittleEndian, &header.CRC); err != nil {
		return nil, err
	}

	// Read header type (1 byte)
	if err := binary.Read(stream, binary.LittleEndian, &header.Type); err != nil {
		return nil, err
	}

	// Read header flags (2 bytes)
	if err := binary.Read(stream, binary.LittleEndian, &header.Flags); err != nil {
		return nil, err
	}

	// Read header size (2 bytes)
	if err := binary.Read(stream, binary.LittleEndian, &header.HeadSize); err != nil {
		return nil, err
	}

	// Check for zero padding (common at end of RAR files)
	// If we read all zeros, Type will be 0 and HeadSize will be 0
	if header.HeadSize == 0 && header.Type == 0 {
		return nil, io.EOF
	}

	// Validate header size - minimum is 7 bytes
	if header.HeadSize < 7 {
		return nil, fmt.Errorf("invalid RAR4 header size: %d (minimum is 7)", header.HeadSize)
	}

	// Check for marker block or archive header which might have HeadSize of exactly 7
	// For these blocks, there's no additional data
	if header.HeadSize == 7 && (header.Type == RAR4HeaderTypeMarker || header.Type == RAR4HeaderTypeArchive) {
		// This is valid for marker/archive blocks with minimal headers
		return &header, nil
	}

	// CRITICAL FIX: Do NOT strip AddSize from the header data.
	// In RAR4 format, LONG_BLOCK means PackedSize(4) + UnpackedSize(4) are present.
	// These fields are part of the header body and must be read into header.Data
	// so that parseRAR4FileHeader can read them correctly.

	baseHeaderSize := 7

	// Sanity check
	const maxHeaderSize = uint16(65535)
	if header.HeadSize > maxHeaderSize {
		return nil, fmt.Errorf("invalid RAR4 header size: %d", header.HeadSize)
	}

	// Read remaining header data
	remainingSize := int(header.HeadSize) - baseHeaderSize
	if remainingSize < 0 {
		return nil, fmt.Errorf("invalid RAR4 header size calculation: remaining=%d", remainingSize)
	}

	if remainingSize > 0 {
		header.Data = make([]byte, remainingSize)
		if _, err := io.ReadFull(stream, header.Data); err != nil {
			return nil, err
		}
	}

	return &header, nil
}
