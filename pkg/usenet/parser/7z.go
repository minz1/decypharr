package parser

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/javi11/sevenzip"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// SevenZParser parses 7z archives from NNTP segments.
type SevenZParser struct {
	source    ArticleSource
	logger    zerolog.Logger
	rarParser *RARParser
}

// NewSevenZParser creates a new 7z parser.
func NewSevenZParser(source ArticleSource, maxConcurrent int, logger zerolog.Logger) *SevenZParser {
	return &SevenZParser{
		source:    source,
		logger:    logger.With().Str("component", "7z_parser").Logger(),
		rarParser: NewRARParser(source, maxConcurrent, logger.With().Str("component", "rar_parser_embedded").Logger()),
	}
}

func (p *SevenZParser) Process(ctx context.Context, group *FileGroup, password string) ([]*storage.NZBFile, error) {
	sort.Slice(group.Files, func(i, j int) bool {
		return group.Files[i].Filename < group.Files[j].Filename
	})

	volumes, err := buildArchiveVolumeDescriptors(group)
	if err != nil {
		return nil, err
	}
	if len(volumes) == 0 {
		return nil, fmt.Errorf("no volumes built from group")
	}

	baseSegments, volumeInfos, err := buildBaseSegments(group)
	if err != nil {
		return nil, err
	}
	if len(baseSegments) == 0 {
		return nil, fmt.Errorf("no base segments built from group")
	}
	segmentIndex, err := newSegmentLayout(baseSegments)
	if err != nil {
		return nil, fmt.Errorf("index 7z source segments: %w", err)
	}
	if validateVolumesErr := segmentIndex.validateVolumes(volumeInfos); validateVolumesErr != nil {
		return nil, fmt.Errorf("validate 7z volume layout: %w", validateVolumesErr)
	}

	readerAt, archiveSize, err := newArticleReaderAt(ctx, p.source, volumes)
	if err != nil {
		return nil, fmt.Errorf("failed to create archive reader: %w", err)
	}

	reader, err := sevenzip.NewReaderWithPassword(readerAt, archiveSize, password)
	if err != nil {
		return nil, fmt.Errorf("failed to open sevenzip reader: %w", err)
	}

	fileList, err := reader.ListFilesWithOffsets()
	if err != nil {
		return nil, fmt.Errorf("failed to list files with offsets: %w", err)
	}

	rarFiles, nonRARFiles := splitStreamable7zEntries(fileList)

	var files []*storage.NZBFile

	// Parse RAR files by reading their headers directly from readerAt
	if len(rarFiles) > 0 {
		rarNZBFiles, rarErr := p.processRARFilesFromPositions(rarFiles, group, readerAt, segmentIndex, password)
		if rarErr != nil {
			return nil, fmt.Errorf("process RAR files embedded in 7z: %w", rarErr)
		}
		files = append(files, rarNZBFiles...)
	}

	// Parse non-RAR files as regular files
	for _, file := range nonRARFiles {
		nzbFile, ok, plainErr := sevenZPlainFile(file, group, segmentIndex, password)
		if plainErr != nil {
			return nil, plainErr
		}
		if ok {
			files = append(files, nzbFile)
		}
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no files found in 7z archive")
	}

	p.logger.Info().
		Int("total_extracted", len(files)).
		Msg("7z archive processing complete")

	return files, nil
}

// sevenZPlainFile maps a stored 7z member onto the raw segments; ok is false
// for an unusable member path.
func sevenZPlainFile(
	file sevenzip.FileInfo,
	group *FileGroup,
	segmentIndex *segmentLayout,
	password string,
) (*storage.NZBFile, bool, error) {
	internal := NormalizeArchivePath(file.Name)
	if internal == "" {
		return nil, false, nil
	}
	name := utils.RemoveInvalidChars(filepath.Base(internal))
	if name == "" {
		name = path.Base(internal)
	}

	// Slice segments for this file's byte range using offset from sevenzip
	if file.Offset < 0 || file.Size == 0 || file.Size > math.MaxInt64 {
		return nil, false, fmt.Errorf("7z file %q has no usable source range", internal)
	}
	size := int64(file.Size)
	segments, err := segmentIndex.slice(file.Offset, size, true)
	if err == nil && len(segments) == 0 {
		err = fmt.Errorf("no source segments overlap the file range")
	}
	if err != nil {
		return nil, false, fmt.Errorf("map 7z file %q to raw source: %w", internal, err)
	}

	return &storage.NZBFile{
		Name:         name,
		InternalPath: internal,
		Size:         size,
		IsStored:     true,
		Groups:       getGroupsList(group.Groups),
		Segments:     segments,
		Password:     password,
		FileType:     storage.NZBFileTypeSevenZip,
	}, true, nil
}

// processRARFilesFromPositions creates volume descriptors for RAR files based on their positions
// within the 7z archive and passes them to the RAR parser.
func (p *SevenZParser) processRARFilesFromPositions(
	rarFiles []sevenzip.FileInfo,
	group *FileGroup,
	readerAt io.ReaderAt,
	segmentIndex *segmentLayout,
	password string,
) ([]*storage.NZBFile, error) {
	if len(rarFiles) == 0 {
		return nil, nil
	}

	// Order the volumes logically (.rar, .r00, .r01, ... or .partN.rar).
	// Their physical order inside the 7z is often different (.r00 ... .rar),
	// and a file spanning volumes is joined in logical order.
	rarFiles = slices.Clone(rarFiles)
	slices.SortStableFunc(rarFiles, func(a, b sevenzip.FileInfo) int {
		if order := cmp.Compare(
			getRARVolumeOrder(filepath.Base(a.Name)),
			getRARVolumeOrder(filepath.Base(b.Name)),
		); order != 0 {
			return order
		}
		return cmp.Compare(a.Offset, b.Offset)
	})
	// Detect RAR version from the first volume
	firstRAR := rarFiles[0]
	versionBuf := make([]byte, len(RAR5Signature))
	if _, err := readerAt.ReadAt(versionBuf, firstRAR.Offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("failed to read RAR signature: %w", err)
	}
	version := detectRARVersion(versionBuf)
	if version == RARVersionUnknown {
		return nil, fmt.Errorf("unknown RAR format in 7z")
	}

	allRawFiles := p.scanEmbeddedRARHeaders(rarFiles, readerAt, version, password)
	if len(allRawFiles) == 0 {
		return nil, fmt.Errorf("no files found in RAR volumes")
	}

	// Aggregate file parts across volumes (files spanning multiple volumes will have multiple entries)
	rarFileEntries := p.rarParser.aggregateFileParts(allRawFiles)

	// Build a map of RAR filename -> offset in 7z
	rarFileOffsets := make(map[string]int64)
	for _, rarFile := range rarFiles {
		rarFileOffsets[filepath.Base(rarFile.Name)] = rarFile.Offset
	}

	// Build NZBFile list; only stored RAR members can be streamed.
	var files []*storage.NZBFile
	for _, rarEntry := range rarFileEntries {
		if rarEntry.IsDirectory || !rarEntry.IsStored {
			continue
		}
		file, err := p.embeddedRARFile(rarEntry, rarFileOffsets, segmentIndex)
		if err != nil {
			return nil, err
		}
		file.Groups = getGroupsList(group.Groups)
		file.Password = password
		files = append(files, file)
	}

	return files, nil
}

// scanEmbeddedRARHeaders parses the member headers of every volume, in
// logical order. Each volume a file spans carries a header for its part
// there, and a file may also start in a later volume, so no volume can be
// skipped.
func (p *SevenZParser) scanEmbeddedRARHeaders(
	rarFiles []sevenzip.FileInfo,
	readerAt io.ReaderAt,
	version RARVersion,
	password string,
) []*RARFileEntry {
	var allRawFiles []*RARFileEntry
	for volIndex, rarFile := range rarFiles {
		headerData, ok := readEmbeddedRARSnippet(readerAt, rarFile)
		if !ok {
			continue
		}
		volumeName := filepath.Base(rarFile.Name)
		switch version {
		case RARVersion5:
			allRawFiles = append(
				allRawFiles,
				p.rarParser.parseRAR5Headers(headerData, volIndex, volumeName, password)...)
		case RARVersion4:
			allRawFiles = append(allRawFiles, p.rarParser.parseRAR4Headers(headerData, volIndex, volumeName)...)
		case RARVersionUnknown:
			return nil
		}
	}
	return allRawFiles
}

// readEmbeddedRARSnippet reads the head of an embedded volume, where its
// (small) headers live; ok is false when it cannot be read.
func readEmbeddedRARSnippet(readerAt io.ReaderAt, rarFile sevenzip.FileInfo) ([]byte, bool) {
	headerSize := int64(rarSnippetSize)
	if rarFile.Size < rarSnippetSize {
		headerSize = int64(rarFile.Size)
	}
	headerData := make([]byte, headerSize)
	n, err := readerAt.ReadAt(headerData, rarFile.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false
	}
	return headerData[:n], true
}

// embeddedRARFile maps a stored RAR member embedded in a 7z archive.
func (p *SevenZParser) embeddedRARFile(
	rarEntry *RARFileEntry,
	rarFileOffsets map[string]int64,
	segmentIndex *segmentLayout,
) (*storage.NZBFile, error) {
	filename := utils.RemoveInvalidChars(filepath.Base(rarEntry.Name))
	if filename == "" {
		filename = path.Base(rarEntry.Name)
	}

	// get segments for this file by processing all its volume parts
	fileSegments, err := p.buildSegmentsForRARFile(rarEntry, rarFileOffsets, segmentIndex)
	if err != nil {
		return nil, fmt.Errorf("map RAR file %q embedded in 7z: %w", rarEntry.Name, err)
	}
	if len(fileSegments) == 0 {
		return nil, fmt.Errorf("RAR file %q embedded in 7z has no source segments", rarEntry.Name)
	}

	p.logger.Debug().
		Str("file", rarEntry.Name).
		Int("segment_count", len(fileSegments)).
		Int64("file_size", rarEntry.UncompressedSize).
		Msg("Built segments for RAR file in 7z")

	return &storage.NZBFile{
		Name:         filename,
		InternalPath: rarEntry.Name,
		Size:         rarEntry.UncompressedSize,
		IsStored:     true,
		Segments:     fileSegments,
		FileType:     storage.NZBFileTypeRar,
	}, nil
}

// buildSegmentsForRARFile builds the segment list for a file across all RAR volume parts.
func (p *SevenZParser) buildSegmentsForRARFile(
	rarEntry *RARFileEntry,
	rarFileOffsets map[string]int64,
	segmentIndex *segmentLayout,
) ([]storage.NZBSegment, error) {
	if len(rarEntry.VolumeParts) == 0 {
		return nil, fmt.Errorf("no volume parts for file %s", rarEntry.Name)
	}

	var fileSegments []storage.NZBSegment
	var currentFileOffset int64 // Offset within the final extracted file

	// Parse each volume part of this file, in volume order
	sortVolumeParts(rarEntry.VolumeParts)
	for partIdx, part := range rarEntry.VolumeParts {
		if part.PackedSize <= 0 {
			continue
		}

		// get the RAR volume file's offset within the 7z
		rarVolumeName := filepath.Base(part.Name)
		rarVolumeOffset, ok := rarFileOffsets[rarVolumeName]
		if !ok {
			return nil, fmt.Errorf("RAR part %q for %s is absent from the 7z file list", part.Name, rarEntry.Name)
		}

		// The file data starts at: rarVolumeOffset (in 7z) + part.DataOffset (in RAR volume)
		absoluteDataOffset := rarVolumeOffset + part.DataOffset

		// Slice segments from the base 7z segments for this part's data range
		partSegments, err := segmentIndex.slice(absoluteDataOffset, part.PackedSize, false)
		if err != nil {
			return nil, fmt.Errorf("failed to slice segments for part %d of %s: %w", partIdx, rarEntry.Name, err)
		}

		if len(partSegments) == 0 {
			return nil, fmt.Errorf("RAR part %d of %s has no source segments", partIdx, rarEntry.Name)
		}

		// Assign output-file positions cumulatively across parts (same
		// pattern as RARParser.buildSegmentsForFile), then append.
		for i := range partSegments {
			partSegments[i].StartOffset = currentFileOffset
			partSegments[i].EndOffset = currentFileOffset + partSegments[i].Bytes - 1
			currentFileOffset += partSegments[i].Bytes
		}
		fileSegments = append(fileSegments, partSegments...)
	}

	return fileSegments, nil
}

// splitStreamable7zEntries keeps only entries whose bytes sit verbatim in the
// archive (copy coder, no AES) and separates embedded RAR volumes from plain
// files. Compressed or encrypted entries would otherwise be served as raw
// packed/cipher bytes.
func splitStreamable7zEntries(files []sevenzip.FileInfo) ([]sevenzip.FileInfo, []sevenzip.FileInfo) {
	var rarFiles, plainFiles []sevenzip.FileInfo
	for _, file := range files {
		if file.Compressed || file.Encrypted {
			continue
		}
		if isRARFile(file.Name) {
			rarFiles = append(rarFiles, file)
		} else {
			plainFiles = append(plainFiles, file)
		}
	}
	return rarFiles, plainFiles
}

// rarSnippetSize is how much of an embedded RAR volume is read for its
// headers; RAR headers are small, so 64KB is usually enough.
const rarSnippetSize = 64 << 10

// isRARFile checks if a filename is a RAR file.
func isRARFile(filename string) bool {
	lower := strings.ToLower(filename)
	return rarMainPattern.MatchString(lower) ||
		rarPartPattern.MatchString(lower) ||
		rarVolumePattern.MatchString(lower)
}
