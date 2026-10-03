package parser

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/rs/zerolog"
	"github.com/sourcegraph/conc/iter"

	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/types"
)

// RAR format constants.
const (
	RAR5Signature = "Rar!\x1A\x07\x01\x00"
	RAR4Signature = "Rar!\x1A\x07\x00"

	RAR5HeaderTypeMain     = 1
	RAR5HeaderTypeFile     = 2
	RAR5HeaderTypeService  = 3
	RAR5HeaderTypeEncrypt  = 4
	RAR5HeaderTypeEndOfArc = 5

	RAR5HeaderFlagExtraArea     = 0x0001
	RAR5HeaderFlagDataArea      = 0x0002
	RAR5HeaderFlagSkipIfUnknown = 0x0004
	RAR5HeaderFlagDataSector    = 0x0008

	RAR5MainFlagVolume       = 0x0001 // Archive is part of multi-volume set
	RAR5MainFlagVolumeNumber = 0x0002 // Volume number field is present
	RAR5MainFlagSolid        = 0x0004 // Solid archive
	RAR5MainFlagRecovery     = 0x0008 // Recovery record present
	RAR5MainFlagLocked       = 0x0010 // Locked archive

	RAR5FileFlagDirectory      = 0x0001
	RAR5FileFlagHasUnixTime    = 0x0002
	RAR5FileFlagHasCRC32       = 0x0004
	RAR5FileFlagUnpSizeUnknown = 0x0008

	RAR5ExtraTypeEncryption = 0x01 // File encryption record (contains IV)
	RAR5ExtraTypeHash       = 0x02 // File hash record
	RAR5ExtraTypeTime       = 0x03 // Extended time record
	RAR5ExtraTypeVersion    = 0x04 // Version info
	RAR5ExtraTypeRedirect   = 0x05 // Symlink record
	RAR5ExtraTypeOwner      = 0x06 // Unix owner record
	RAR5ExtraTypeService    = 0x07 // Service data

	RAR5CompressionMethodStore = 0

	RAR4HeaderTypeMarker  = 0x72
	RAR4HeaderTypeArchive = 0x73
	RAR4HeaderTypeFile    = 0x74
	RAR4HeaderTypeService = 0x7A
	RAR4HeaderTypeEnd     = 0x7B

	RAR4HeaderFlagHasAdd    = 0x0001
	RAR4HeaderFlagLongBlock = 0x8000
	RAR4FileFlagDirectory   = 0x00E0
	RAR4FileFlagSolid       = 0x0010
	RAR4FileFlagEncrypted   = 0x0004 // File data is encrypted
	RAR4FileFlagHighSize    = 0x0100 // 64-bit file size (high 4 bytes follow after low 4 bytes)
	RAR4ArchiveFlagPassword = 0x0080 // Archive headers are encrypted

	RAR4CompressionMethodStore = 0x30

	// rar5MinHeaderSize is CRC(4) plus the smallest size/type/flags vints.
	rar5MinHeaderSize = 11

	rar4SignatureSize  = 7 // "Rar!\x1A\x07\x00"
	rar4BaseHeaderSize = 7 // CRC(2) + Type(1) + Flags(2) + HeadSize(2)
	rar4SizeFieldLen   = 4
	// rar4MinFileHeaderData is the fixed FILE_HEAD part after the base header:
	// PACK_SIZE, UNP_SIZE, HOST_OS, FILE_CRC, FTIME, UNP_VER, METHOD, NAME_SIZE, ATTR.
	rar4MinFileHeaderData = 25
	rar4FileTimeSize      = 4
	rar4AttributesSize    = 4
	// highWordShift combines HIGH_*_SIZE words with the low 32 bits.
	highWordShift = 32
	// rar4MethodRawStream (0x81) is a compressed method whose files have been
	// reported to play as raw streams.
	rar4MethodRawStream = 0x81
	// rar4HighPackSizeOffset locates HIGH_PACK_SIZE within header data.
	rar4HighPackSizeOffset = rar4MinFileHeaderData
)

// RARVersion represents the RAR format version.
type RARVersion int

const (
	RARVersion4       RARVersion = 4
	RARVersion5       RARVersion = 5
	RARVersionUnknown RARVersion = 0
)

// RARArchiveInfo contains information about the entire RAR archive.
type RARArchiveInfo struct {
	Version           RARVersion
	IsMultiVol        bool
	IsHeaderEncrypted bool   // Headers are encrypted (needs password to list files)
	IsDataEncrypted   bool   // File data is encrypted
	EncryptionKey     []byte // AES-256 key derived from password (32 bytes)
	Files             []*RARFileEntry
}

// RARFileEntry represents a file within the RAR archive.
type RARFileEntry struct {
	Name             string
	UncompressedSize int64
	PackedSize       int64
	DataOffset       int64 // Offset where compressed data starts
	IsStored         bool  // True if stored (method 0), false if compressed
	IsDirectory      bool
	IsEncrypted      bool                   // File data is encrypted
	EncryptionKey    []byte                 // AES-256 key for data decryption (derived from extra area salt)
	EncryptionIV     []byte                 // AES IV for data decryption (16 bytes, from extra area)
	VolumeParts      []*types.RARVolumePart // Parts across volumes
	CRC32            uint32
	VolumeIndex      int // Which volume this file starts in
}

// RARParser handles parsing RAR archives from usenet segments.
type RARParser struct {
	source        ArticleSource
	maxConcurrent int
	logger        zerolog.Logger
}

// NewRARParser creates a new RAR parser.
func NewRARParser(source ArticleSource, maxConcurrent int, logger zerolog.Logger) *RARParser {
	return &RARParser{
		source:        source,
		maxConcurrent: maxConcurrent,
		logger:        logger.With().Str("component", "rar_parser").Logger(),
	}
}

// Process parses a RAR volume group and maps every stored member onto the
// raw article segments.
func (p *RARParser) Process(ctx context.Context, group *FileGroup, password string) ([]*storage.NZBFile, error) {
	p.logger.Debug().
		Str("group", group.BaseName).
		Int("file_count", len(group.Files)).
		Msg("Starting RAR archive processing")

	if len(group.Files) == 0 {
		return nil, fmt.Errorf("no files")
	}

	// Sort RAR files by volume order (.rar first, then .r00, .r01, etc.)
	// For obfuscated filenames that all get the same sort key, fall back to
	// NZB file Number (upload order) which preserves the original volume sequence.
	sort.Slice(group.Files, func(i, j int) bool {
		oi := getRARVolumeOrder(group.Files[i].Filename)
		oj := getRARVolumeOrder(group.Files[j].Filename)
		if oi != oj {
			return oi < oj
		}
		return group.Files[i].Number < group.Files[j].Number
	})

	volumes, err := buildArchiveVolumeDescriptors(group)
	if err != nil {
		return nil, err
	}
	if len(volumes) == 0 {
		return nil, fmt.Errorf("no RAR volumes found")
	}
	layout, err := newArchiveLayout(group)
	if err != nil {
		return nil, err
	}

	// Parse RAR archive to get file entries with volume parts
	archiveInfo, err := p.parseArchive(ctx, volumes, password)
	if err != nil {
		return nil, fmt.Errorf("failed to parse RAR archive: %w", err)
	}

	// Check if archive has encrypted headers and we couldn't parse it
	if archiveInfo.IsHeaderEncrypted && len(archiveInfo.Files) == 0 {
		return nil, fmt.Errorf("RAR archive has encrypted headers; password required or incorrect")
	}

	return p.streamableFiles(archiveInfo, group, layout, password)
}

// archiveLayout indexes a volume group's raw segments and where each volume
// starts in that flat byte space.
type archiveLayout struct {
	segments     *segmentLayout
	volumeStarts map[int]int64
}

func newArchiveLayout(group *FileGroup) (*archiveLayout, error) {
	baseSegments, volumeInfos, err := buildBaseSegments(group)
	if err != nil {
		return nil, err
	}
	if len(baseSegments) == 0 {
		return nil, fmt.Errorf("no base segments found for RAR volumes")
	}
	segmentIndex, err := newSegmentLayout(baseSegments)
	if err != nil {
		return nil, fmt.Errorf("index RAR source segments: %w", err)
	}
	if validateVolumesErr := segmentIndex.validateVolumes(volumeInfos); validateVolumesErr != nil {
		return nil, fmt.Errorf("validate RAR volume layout: %w", validateVolumesErr)
	}
	return &archiveLayout{segments: segmentIndex, volumeStarts: buildVolumeOffsetMap(volumeInfos)}, nil
}

// streamableFiles turns the archive's stored members into NZB files; only
// stored (uncompressed) files can be streamed.
func (p *RARParser) streamableFiles(
	archiveInfo *RARArchiveInfo,
	group *FileGroup,
	layout *archiveLayout,
	password string,
) ([]*storage.NZBFile, error) {
	fallbackName := utils.RemoveInvalidChars(path.Base(group.BaseName))
	files := make([]*storage.NZBFile, 0, len(archiveInfo.Files))
	hasNoneStored := false
	for _, rarFile := range archiveInfo.Files {
		if rarFile.IsDirectory {
			continue
		}
		if !rarFile.IsStored {
			hasNoneStored = true
			continue
		}
		file, err := p.streamableFile(rarFile, layout, password)
		if err != nil {
			return nil, err
		}
		if file.Name == "" {
			file.Name = fallbackName
		}
		file.Groups = getGroupsList(group.Groups)
		// Fall back to the archive key if no file-specific key was derived.
		if len(file.EncryptionKey) == 0 {
			file.EncryptionKey = archiveInfo.EncryptionKey
		}
		files = append(files, file)
	}

	if len(files) == 0 {
		if hasNoneStored {
			return nil, fmt.Errorf("RAR archive contains no stored (uncompressed) files; cannot stream")
		}
		return nil, fmt.Errorf("no valid files found in RAR archive")
	}
	return files, nil
}

// streamableFile maps one stored member across its volume parts.
func (p *RARParser) streamableFile(
	rarFile *RARFileEntry,
	layout *archiveLayout,
	password string,
) (*storage.NZBFile, error) {
	fileSegments, err := p.buildSegmentsForFile(rarFile, layout.segments, layout.volumeStarts)
	if err != nil {
		return nil, fmt.Errorf("map stored RAR file %q: %w", rarFile.Name, err)
	}
	if len(fileSegments) == 0 {
		return nil, fmt.Errorf("stored RAR file %q has no source segments", rarFile.Name)
	}

	var streamSize int64
	for _, seg := range fileSegments {
		streamSize += seg.Bytes
	}
	size := rarFile.UncompressedSize
	if size <= 0 || (streamSize > 0 && size > streamSize) {
		// Clamp to streamable size to avoid advertising bytes we can't serve.
		size = streamSize
	}

	return &storage.NZBFile{
		Name:          utils.RemoveInvalidChars(path.Base(rarFile.Name)),
		InternalPath:  rarFile.Name,
		Segments:      fileSegments, // Direct segment list with offsets!
		Password:      password,
		FileType:      storage.NZBFileTypeRar,
		Size:          size,
		IsStored:      rarFile.IsStored,
		IsEncrypted:   rarFile.IsEncrypted, // Per-file encryption from extra area
		EncryptionKey: rarFile.EncryptionKey,
		EncryptionIV:  rarFile.EncryptionIV, // Per-file IV from extra area
	}, nil
}

// rarVolumeResult is one volume's parse outcome.
type rarVolumeResult struct {
	index             int
	files             []*RARFileEntry
	isHeaderEncrypted bool
	encryptionKey     []byte // AES-256 key for encrypted file data
	err               error
}

// parseArchive parses every volume in parallel and aggregates the members.
func (p *RARParser) parseArchive(
	ctx context.Context,
	volumes []*types.Volume,
	password string,
) (*RARArchiveInfo, error) {
	if len(volumes) == 0 {
		return nil, fmt.Errorf("no volumes provided")
	}

	// Detect RAR version from first volume
	firstStream := newRarReader(ctx, p.source, []*types.Volume{volumes[0]})
	sig := make([]byte, len(RAR5Signature))
	if _, err := io.ReadFull(firstStream, sig); err != nil {
		return nil, fmt.Errorf("failed to read RAR signature: %w", err)
	}
	version := detectRARVersion(sig)
	if version == RARVersionUnknown {
		return nil, fmt.Errorf("unknown RAR format")
	}

	indices := make([]int, len(volumes))
	for i := range indices {
		indices[i] = i
	}
	mapper := iter.Mapper[int, rarVolumeResult]{MaxGoroutines: min(len(volumes), p.maxConcurrent)}
	results := mapper.Map(indices, func(idx *int) rarVolumeResult {
		return p.parseVolume(ctx, volumes[*idx], *idx, version, password)
	})

	var allRawFiles []*RARFileEntry
	var encryptionKey []byte
	isHeaderEncrypted := false
	for _, result := range results { // Map preserves input order
		if result.err != nil {
			return nil, fmt.Errorf("parse RAR volume %d (%s): %w", result.index, volumes[result.index].Name, result.err)
		}
		isHeaderEncrypted = isHeaderEncrypted || result.isHeaderEncrypted
		if len(encryptionKey) == 0 {
			encryptionKey = result.encryptionKey
		}
		allRawFiles = append(allRawFiles, result.files...)
	}

	archiveInfo := &RARArchiveInfo{
		Version:           version,
		IsMultiVol:        len(volumes) > 1,
		IsHeaderEncrypted: isHeaderEncrypted,
	}
	// If headers are encrypted, we can't list files without password
	if isHeaderEncrypted && len(allRawFiles) == 0 {
		return archiveInfo, nil
	}
	if len(allRawFiles) == 0 {
		return nil, fmt.Errorf("no files found in any RAR volume")
	}

	// Files that span multiple volumes have one entry per volume; merge them.
	archiveInfo.IsDataEncrypted = len(encryptionKey) > 0
	archiveInfo.EncryptionKey = encryptionKey
	archiveInfo.Files = p.aggregateFileParts(allRawFiles)
	return archiveInfo, nil
}

// parseVolume parses one volume's headers from its own stream.
func (p *RARParser) parseVolume(
	ctx context.Context,
	vol *types.Volume,
	volIdx int,
	version RARVersion,
	password string,
) rarVolumeResult {
	stream := newRarReader(ctx, p.source, []*types.Volume{vol})

	// Skip the signature (7 or 8 bytes depending on version)
	sigSize := len(RAR5Signature)
	if version == RARVersion4 {
		sigSize = len(RAR4Signature)
	}
	if _, err := io.ReadFull(stream, make([]byte, sigSize)); err != nil {
		return rarVolumeResult{index: volIdx, err: err}
	}

	switch version {
	case RARVersion5:
		result, err := p.parseRAR5Stream(stream, volIdx, vol.Name, password)
		if err != nil {
			return rarVolumeResult{index: volIdx, err: err}
		}
		return rarVolumeResult{
			index:             volIdx,
			files:             result.Files,
			isHeaderEncrypted: result.IsHeaderEncrypted,
			encryptionKey:     result.EncryptionKey,
		}
	case RARVersion4:
		files, err := p.parseRAR4Stream(stream, volIdx, vol.Name, vol.Size)
		return rarVolumeResult{index: volIdx, files: files, err: err}
	case RARVersionUnknown:
	}
	return rarVolumeResult{index: volIdx, err: fmt.Errorf("unsupported RAR version: %d", version)}
}

// detectRARVersion detects RAR version from signature.
func detectRARVersion(data []byte) RARVersion {
	if len(data) >= 8 && bytes.Equal(data[:8], []byte(RAR5Signature)) {
		return RARVersion5
	}
	if len(data) >= 7 && bytes.Equal(data[:7], []byte(RAR4Signature)) {
		return RARVersion4
	}
	return RARVersionUnknown
}

// parseRAR5Headers parses the RAR 5.0 headers of an in-memory volume
// snippet, tracking absolute offsets and skipping data areas. Parsing stops at
// the end of archive, at a data area that extends past the snippet, or at an
// unreadable header (the snippet boundary or corrupt data).
func (p *RARParser) parseRAR5Headers(
	data []byte,
	volumeIndex int,
	volumeName string,
	password string,
) []*RARFileEntry {
	if len(data) < len(RAR5Signature) {
		return nil
	}
	r := bytes.NewReader(data[len(RAR5Signature):])
	vol := rar5Volume{index: volumeIndex, name: volumeName, password: password}
	result := &parseRAR5StreamResult{}
	currentOffset := int64(len(RAR5Signature)) // absolute position in the volume

	for r.Len() >= rar5MinHeaderSize {
		header, headerSize, dataSize, err := p.readRAR5Header(r)
		if err != nil {
			p.logUnexpectedRAR5HeaderError(err)
			break
		}

		// Data starts immediately after the header
		dataOffset := currentOffset + int64(headerSize)
		if header.Type == RAR5HeaderTypeFile {
			p.appendRAR5File(result, header, vol, dataOffset, dataSize)
		}
		currentOffset = dataOffset + dataSize

		// The next header is only readable if this data area fits the snippet.
		if header.Type == RAR5HeaderTypeEndOfArc || dataSize > int64(r.Len()) {
			break
		}
		_, _ = r.Seek(dataSize, io.SeekCurrent) // in range: checked above
	}
	return result.Files
}

// logUnexpectedRAR5HeaderError logs header errors other than the expected
// snippet-boundary ones (EOF, sizes that run past the snippet).
func (p *RARParser) logUnexpectedRAR5HeaderError(err error) {
	msg := err.Error()
	if errors.Is(err, io.EOF) || strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "invalid header size") || strings.Contains(msg, "too large") {
		return
	}
	p.logger.Debug().Err(err).Msg("Unexpected error reading RAR5 header")
}

// rar5HeaderData represents a RAR 5.0 header.
type rar5HeaderData struct {
	ExtraSize uint64
	Type      uint64
	Flags     uint64
	Data      []byte
}

// rar5FileFields are the fixed FILE header fields before the extra area.
type rar5FileFields struct {
	flags           uint64
	unpackedSize    uint64
	compressionInfo uint64
	crc32           uint32
	name            []byte
}

// RAR5 FILE header layout limits.
const (
	// rar5FileFlagsMask covers the defined flags (directory, unix time, CRC32,
	// unknown size); any other bit means we are reading garbage, e.g. from a
	// continuation volume without file headers.
	rar5FileFlagsMask = 0x0F
	rar5UnixTimeSize  = 4
	maxRAR5NameLength = 4096
	// compression_info bits 7-9 hold the method; 0 is stored.
	rar5CompressionMethodMask  = 0x0380
	rar5CompressionMethodShift = 7
)

// readRAR5FileFields reads the FILE header fields; ok is false on garbage or
// truncated data.
func readRAR5FileFields(r *bytes.Reader) (rar5FileFields, bool) {
	var f rar5FileFields
	var err error
	if f.flags, err = readVInt(r); err != nil || f.flags > rar5FileFlagsMask {
		return f, false
	}
	if f.unpackedSize, err = readVInt(r); err != nil {
		return f, false
	}
	if _, err = readVInt(r); err != nil { // file attributes
		return f, false
	}
	if f.flags&RAR5FileFlagHasUnixTime != 0 {
		_, _ = r.Seek(rar5UnixTimeSize, io.SeekCurrent) // a short read fails below
	}
	if f.flags&RAR5FileFlagHasCRC32 != 0 && binary.Read(r, binary.LittleEndian, &f.crc32) != nil {
		return f, false
	}
	if f.compressionInfo, err = readVInt(r); err != nil {
		return f, false
	}
	if _, err = readVInt(r); err != nil { // host OS
		return f, false
	}
	nameLength, err := readVInt(r)
	if err != nil || nameLength > maxRAR5NameLength {
		return f, false
	}
	f.name = make([]byte, nameLength)
	if _, err = io.ReadFull(r, f.name); err != nil {
		return f, false
	}
	return f, true
}

// validRAR5Name rejects names with control characters other than tab and
// newlines, which only garbage data produces.
func validRAR5Name(name []byte) bool {
	for _, b := range name {
		if b < ' ' && b != '\t' && b != '\n' && b != '\r' {
			return false
		}
	}
	return true
}

// isDirectory trusts the directory flag only for empty entries without a
// media extension: split files and garbage data set it improperly.
func (f rar5FileFields) isDirectory(filename string) bool {
	if f.flags&RAR5FileFlagDirectory == 0 || f.unpackedSize > 0 {
		return false
	}
	return filename == "" || !utils.IsMediaFile(filename)
}

// isStored reports compression method 0 (no compression).
func (f rar5FileFields) isStored() bool {
	return (f.compressionInfo&rar5CompressionMethodMask)>>rar5CompressionMethodShift == 0
}

// parseRAR5FileHeader parses a RAR 5.0 file header
// If password is provided and encryption salt is found, it derives the file-specific encryption key.
func (p *RARParser) parseRAR5FileHeader(
	data []byte,
	extraSize uint64,
	volumeIndex int,
	volumeName string,
	dataOffset int64,
	packedSize int64,
	password string,
) *RARFileEntry {
	if extraSize > math.MaxInt32 || int(extraSize) > len(data) {
		return nil
	}
	baseEnd := len(data) - int(extraSize)
	fields, ok := readRAR5FileFields(bytes.NewReader(data[:baseEnd]))
	if !ok || !validRAR5Name(fields.name) {
		return nil
	}
	// Sanitize filename to ensure valid UTF-8
	// This prevents "string field contains invalid UTF-8" errors during NZB marshaling
	filename := strings.ToValidUTF8(string(fields.name), "")
	unpackedSize, crc32 := fields.unpackedSize, fields.crc32
	isDirectory := fields.isDirectory(filename)
	isStored := fields.isStored()

	// An out-of-range (or "unknown") unpacked size is left at 0; Process
	// then advertises the streamable size instead.
	uncompressedSize, sizeErr := rar5Size(unpackedSize)
	if sizeErr != nil {
		uncompressedSize = 0
	}

	encryption, err := parseRAR5Extra(data[baseEnd:], password)
	if err != nil {
		return nil
	}

	return &RARFileEntry{
		Name:             filename,
		UncompressedSize: uncompressedSize,
		PackedSize:       packedSize,
		DataOffset:       dataOffset,
		IsStored:         isStored,
		IsDirectory:      isDirectory,
		IsEncrypted:      encryption.Encrypted,
		EncryptionKey:    encryption.Key,
		EncryptionIV:     encryption.IV,
		CRC32:            crc32,
		VolumeIndex:      volumeIndex,
		VolumeParts: []*types.RARVolumePart{{
			Name:         volumeName,
			DataOffset:   dataOffset,
			PackedSize:   packedSize,
			UnpackedSize: packedSize, // Use PackedSize - represents data IN THIS VOLUME PART, not full file
			Stored:       isStored,
			PartNumber:   volumeIndex, // Set part number to volume index
		}},
	}
}

// parseRAR4Headers parses the RAR 4.x headers of an in-memory volume
// snippet. Parsing stops at the end of archive, at a data area that extends
// past the snippet, or at an unreadable header.
func (p *RARParser) parseRAR4Headers(data []byte, volumeIndex int, volumeName string) []*RARFileEntry {
	if len(data) < rar4SignatureSize {
		return nil
	}
	r := bytes.NewReader(data)
	_, _ = r.Seek(rar4SignatureSize, io.SeekStart) // in range: checked above

	var files []*RARFileEntry
	currentOffset := int64(rar4SignatureSize)

	// Continue while at least one minimal header may remain.
	for r.Len() >= rar4BaseHeaderSize {
		header, ok := p.nextRAR4Header(r)
		if !ok {
			break
		}
		dataSize, ok := rar4DataSize(header)
		if !ok {
			break
		}

		if header.Type == RAR4HeaderTypeFile {
			file := p.parseRAR4FileHeader(header, volumeIndex, volumeName, currentOffset+int64(header.HeadSize))
			if file != nil {
				files = append(files, file)
			}
		}

		// Move to the next header if it is still inside the snippet; beyond
		// it lies file data we do not need.
		nextOffset := currentOffset + int64(header.HeadSize) + dataSize
		if header.Type == RAR4HeaderTypeEnd || nextOffset <= currentOffset || nextOffset > int64(len(data)) {
			break
		}
		currentOffset = nextOffset
		_, _ = r.Seek(currentOffset, io.SeekStart) // in range: checked above
	}

	return files
}

// nextRAR4Header reads one header; ok is false when none can be read.
func (p *RARParser) nextRAR4Header(r io.Reader) (*rar4Header, bool) {
	header, err := p.readRAR4HeaderFromStream(r)
	return header, err == nil
}

// rar4Header represents a RAR 4.x block header. Data holds every header byte
// after the 7-byte base header, including the ADD_SIZE field of LONG_BLOCK
// headers (for file headers ADD_SIZE is PACK_SIZE).
type rar4Header struct {
	CRC      uint16
	Type     uint8
	Flags    uint16
	HeadSize uint16
	Data     []byte
}

// rar4DataSize returns the size of the data area that follows a RAR4 block.
// Only LONG_BLOCK headers carry one; for file and service headers it is
// PACK_SIZE, whose high half follows the fixed fields when HIGH_SIZE is set.
// ok is false when the header is too short to hold the declared size.
func rar4DataSize(header *rar4Header) (int64, bool) {
	if header.Flags&RAR4HeaderFlagLongBlock == 0 {
		return 0, true
	}
	if len(header.Data) < rar4SizeFieldLen {
		return 0, false
	}
	size := int64(binary.LittleEndian.Uint32(header.Data))
	isFileLike := header.Type == RAR4HeaderTypeFile || header.Type == RAR4HeaderTypeService
	if !isFileLike || header.Flags&RAR4FileFlagHighSize == 0 {
		return size, true
	}
	if len(header.Data) < rar4HighPackSizeOffset+rar4SizeFieldLen {
		return 0, false
	}
	high := binary.LittleEndian.Uint32(header.Data[rar4HighPackSizeOffset:])
	if high > math.MaxInt32 {
		return 0, false
	}
	return int64(high)<<highWordShift | size, true
}

// parseRAR4FileHeader parses RAR 4.x file header.
func (p *RARParser) parseRAR4FileHeader(
	header *rar4Header,
	volumeIndex int,
	volumeName string,
	dataOffset int64,
) *RARFileEntry {
	if len(header.Data) < rar4MinFileHeaderData {
		return nil
	}

	r := bytes.NewReader(header.Data)

	// Read packed size (4 bytes low word)
	var packedSizeLow uint32
	_ = binary.Read(r, binary.LittleEndian, &packedSizeLow)

	// Read unpacked size (4 bytes low word)
	var unpackedSizeLow uint32
	_ = binary.Read(r, binary.LittleEndian, &unpackedSizeLow)

	// Read host OS (1 byte)
	_, _ = r.Seek(1, io.SeekCurrent)

	// Read file CRC (4 bytes)
	var crc32 uint32
	_ = binary.Read(r, binary.LittleEndian, &crc32)

	// Read file time (4 bytes)
	_, _ = r.Seek(rar4FileTimeSize, io.SeekCurrent)

	// Read RAR version (1 byte)
	_, _ = r.Seek(1, io.SeekCurrent)

	// Read compression method (1 byte)
	var method uint8
	_ = binary.Read(r, binary.LittleEndian, &method)

	// Read name length (2 bytes)
	var nameLength uint16
	_ = binary.Read(r, binary.LittleEndian, &nameLength)

	// Read file attributes (4 bytes)
	_, _ = r.Seek(rar4AttributesSize, io.SeekCurrent)

	// Handle HIGH_SIZE flag - read high 32 bits of sizes
	var packedSize, unpackedSize int64
	if header.Flags&RAR4FileFlagHighSize != 0 {
		// Read high 4 bytes of packed size
		var packedSizeHigh uint32
		_ = binary.Read(r, binary.LittleEndian, &packedSizeHigh)
		// Read high 4 bytes of unpacked size
		var unpackedSizeHigh uint32
		_ = binary.Read(r, binary.LittleEndian, &unpackedSizeHigh)
		if packedSizeHigh > math.MaxInt32 || unpackedSizeHigh > math.MaxInt32 {
			return nil // sizes beyond int64 are corrupt
		}

		packedSize = int64(packedSizeHigh)<<highWordShift | int64(packedSizeLow)
		unpackedSize = int64(unpackedSizeHigh)<<highWordShift | int64(unpackedSizeLow)
	} else {
		packedSize = int64(packedSizeLow)
		unpackedSize = int64(unpackedSizeLow)
	}

	// Read filename
	if nameLength == 0 {
		return nil
	}
	nameBytes := make([]byte, nameLength)
	if _, err := io.ReadFull(r, nameBytes); err != nil {
		return nil
	}

	// Check if directory
	isDirectory := (header.Flags & RAR4FileFlagDirectory) == RAR4FileFlagDirectory

	// Check if stored (method 0x30)
	// User report: Method 0x81 (Compressed) files play as raw streams, suggesting they are effectively stored
	// or the player handles the compression. To support seeking, we must treat them as stored
	// and ensure UncompressedSize matches PackedSize so we don't advertise data we can't serve.
	isStored := method == RAR4CompressionMethodStore || method == rar4MethodRawStream

	if isStored && method != RAR4CompressionMethodStore {
		unpackedSize = packedSize
	}

	return &RARFileEntry{
		Name:             strings.ToValidUTF8(string(nameBytes), ""),
		UncompressedSize: unpackedSize,
		PackedSize:       packedSize,
		DataOffset:       dataOffset,
		IsStored:         isStored,
		IsDirectory:      isDirectory,
		CRC32:            crc32,
		VolumeIndex:      volumeIndex,
		VolumeParts: []*types.RARVolumePart{{
			Name:         volumeName,
			DataOffset:   dataOffset,
			PackedSize:   packedSize,
			UnpackedSize: packedSize, // Use PackedSize - represents data IN THIS VOLUME PART, not full file
			Stored:       isStored,
			PartNumber:   volumeIndex, // Set part number to volume index
		}},
	}
}

// buildVolumeOffsetMap builds a map of cumulative offsets for each volume.
func buildVolumeOffsetMap(volumeInfos []storage.ArchiveVolumeInfo) map[int]int64 {
	offsetMap := make(map[int]int64)
	var cumulativeOffset int64

	for i, volInfo := range volumeInfos {
		offsetMap[i] = cumulativeOffset
		cumulativeOffset += volInfo.Size
	}

	return offsetMap
}

// buildSegmentsForFile builds the segment list for a file across all its RAR volume parts
// CRITICAL: part.DataOffset is the offset WITHIN the decoded RAR volume file
// We need to map this to the actual NNTP segment that contains that byte.
func (p *RARParser) buildSegmentsForFile(
	rarFile *RARFileEntry,
	segmentIndex *segmentLayout,
	volumeOffsetMap map[int]int64,
) ([]storage.NZBSegment, error) {
	if len(rarFile.VolumeParts) == 0 {
		return nil, fmt.Errorf("no volume parts for file %s", rarFile.Name)
	}

	// Ensure volume parts are ordered by volume index and data offset.
	sortVolumeParts(rarFile.VolumeParts)

	var fileSegments []storage.NZBSegment
	var currentFileOffset int64 // Offset within the final extracted file

	// Parse each volume part of this file
	for _, part := range rarFile.VolumeParts {
		if part.UnpackedSize <= 0 {
			continue
		}

		// Build segments for this specific part
		// part.DataOffset = offset within the RAR volume where this file's data starts
		// part.UnpackedSize = how many bytes of the file are in this part (this is what we stream!)
		partSegments, err := p.buildSegmentsForIndexedVolumePart(part, segmentIndex, volumeOffsetMap)
		if err != nil {
			return nil, fmt.Errorf("map volume part %d of %s: %w", part.PartNumber, rarFile.Name, err)
		}

		if len(partSegments) == 0 {
			return nil, fmt.Errorf("volume part %d of %s has no source segments", part.PartNumber, rarFile.Name)
		}

		// Append with correct file offsets
		for i := range partSegments {
			partSegments[i].StartOffset = currentFileOffset
			partSegments[i].EndOffset = currentFileOffset + partSegments[i].Bytes - 1
			currentFileOffset += partSegments[i].Bytes
		}
		fileSegments = append(fileSegments, partSegments...)
	}

	if len(fileSegments) == 0 {
		return nil, fmt.Errorf("no segments built for file %s", rarFile.Name)
	}

	// Ensure segments are ordered by output offsets for streaming correctness.
	byOutput := func(a, b storage.NZBSegment) int { return cmp.Compare(a.StartOffset, b.StartOffset) }
	if !slices.IsSortedFunc(fileSegments, byOutput) {
		slices.SortFunc(fileSegments, byOutput)
	}

	return fileSegments, nil
}

// sortVolumeParts orders parts by volume, then by data offset.
func sortVolumeParts(parts []*types.RARVolumePart) {
	byVolume := func(a, b *types.RARVolumePart) int {
		if order := cmp.Compare(a.PartNumber, b.PartNumber); order != 0 {
			return order
		}
		return cmp.Compare(a.DataOffset, b.DataOffset)
	}
	if !slices.IsSortedFunc(parts, byVolume) {
		slices.SortStableFunc(parts, byVolume)
	}
}

func (p *RARParser) buildSegmentsForIndexedVolumePart(
	part *types.RARVolumePart,
	segmentIndex *segmentLayout,
	volumeOffsetMap map[int]int64,
) ([]storage.NZBSegment, error) {
	// get the absolute byte offset where this volume starts in the flat segment list
	volumeStartOffset, ok := volumeOffsetMap[part.PartNumber]
	if !ok {
		return nil, fmt.Errorf("volume offset not found for part number %d", part.PartNumber)
	}

	// Calculate absolute offset in the flat segment space
	// volumeStartOffset = cumulative size of all previous volumes
	// part.DataOffset = offset within THIS volume where the file data starts
	absoluteStartOffset := volumeStartOffset + part.DataOffset
	if absoluteStartOffset < 0 {
		return nil, fmt.Errorf("invalid volume start offset %d", absoluteStartOffset)
	}
	return segmentIndex.slice(absoluteStartOffset, part.UnpackedSize, false)
}

// aggregateFileParts combines file parts across volumes for multi-volume RAR archives
// When a file spans multiple RAR volumes, each volume contains a file header for the continuation
// This function merges these into a single RARFileEntry with all volume parts.
func (p *RARParser) aggregateFileParts(rawFiles []*RARFileEntry) []*RARFileEntry {
	if len(rawFiles) == 0 {
		return nil
	}

	// Map of filename -> aggregated file entry
	fileMap := make(map[string]*RARFileEntry)

	for _, file := range rawFiles {
		if file == nil {
			continue
		}

		existing, found := fileMap[file.Name]
		if !found {
			// First occurrence of this file
			fileMap[file.Name] = file
		} else if len(file.VolumeParts) > 0 {
			// File continuation from another volume - merge the parts
			// (PartNumber already set correctly during parsing).
			existing.VolumeParts = append(existing.VolumeParts, file.VolumeParts...)
			existing.PackedSize += file.PackedSize
		}
	}

	// Convert map back to slice
	result := make([]*RARFileEntry, 0, len(fileMap))
	for _, file := range fileMap {
		result = append(result, file)
	}

	// Sort by name for consistent ordering
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result
}
