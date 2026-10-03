package parser

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/manifest"
	"github.com/sirrobot01/decypharr/pkg/usenet/types"
)

// getRARVolumeOrder returns a sort key for RAR volume ordering.
// .rar or .part01.rar = 0 (first volume)
// .r00 = 1, .r01 = 2, etc.; legacy sequences may continue at .s00.
// .part02.rar = 2, .part03.rar = 3, etc.
func getRARVolumeOrder(filename string) int {
	lower := strings.ToLower(filename)
	ext := filepath.Ext(lower)
	base := strings.TrimSuffix(lower, ext)

	// Old-style naming: .rar, .r00, .r01, ...
	if ext == ".rar" {
		// Check for .partXX.rar pattern (new style)
		if matches := rarPartNumberPattern.FindStringSubmatch(base); len(matches) == 2 {
			num, _ := strconv.Atoi(matches[1])
			return num // .part01.rar = 1, .part02.rar = 2
		}
		// Plain .rar is the first volume
		return 0
	}

	// .rXX/.rXXX pattern (old style continuation)
	if (len(ext) == 4 || len(ext) == 5) && strings.HasPrefix(ext, ".r") {
		numStr := ext[2:]
		if num, err := strconv.Atoi(numStr); err == nil {
			return num + 1 // .r00 = 1, .r01 = 2, etc.
		}
	}

	// After .r99, classic RAR naming continues with .s00, .t00, ... .z99.
	if len(ext) == 4 && ext[0] == '.' && ext[1] >= 's' && ext[1] <= 'z' {
		if num, err := strconv.Atoi(ext[2:]); err == nil {
			return 101 + int(ext[1]-'s')*100 + num
		}
	}

	// Unknown pattern, put at end
	return 999999
}

// getZIPVolumeOrder puts split volumes (.z01, .z02, ..., .z100) before the
// terminal .zip file. Numeric comparison avoids lexicographic z100/z99 bugs.
func getZIPVolumeOrder(filename string) int {
	lower := strings.ToLower(filename)
	if strings.HasSuffix(lower, ".zip") {
		return int(^uint(0) >> 1)
	}
	ext := filepath.Ext(lower)
	if zipPartPattern.MatchString(ext) {
		if num, err := strconv.Atoi(strings.TrimPrefix(ext, ".z")); err == nil {
			return num
		}
	}
	return int(^uint(0)>>1) - 1
}

func wrapNZBFile(f *storage.NZBFile) ([]*storage.NZBFile, error) {
	if f == nil {
		return nil, fmt.Errorf("nzb file is nil")
	}
	return []*storage.NZBFile{f}, nil
}

// fileMetaKey returns a stable key for associating per-file metadata.
func fileMetaKey(file manifest.File) string {
	if len(file.Segments) > 0 {
		return "m:" + file.Segments[0].MessageID
	}
	if file.Subject != "" {
		return "s:" + file.Subject
	}
	if file.Number > 0 {
		return fmt.Sprintf("n:%d", file.Number)
	}
	return ""
}

func getGroupsList(groups map[string]struct{}) []string {
	result := make([]string, 0, len(groups))
	for g := range groups {
		result = append(result, g)
	}
	sort.Strings(result)
	return result
}

func determineNZBName(filename string, meta map[string]string) string {
	// Prefer filename if it exists
	if filename != "" {
		filename = strings.TrimSuffix(filename, filepath.Ext(filename))
	} else if name := meta["Name"]; name != "" {
		filename = name
	} else if title := meta["title"]; title != "" {
		filename = title
	}
	return utils.RemoveInvalidChars(filename)
}

func determineExtension(group *FileGroup) string {
	// Try to determine extension from filenames
	for _, file := range group.Files {
		ext := filepath.Ext(file.Filename)
		if ext != "" {
			return ext
		}
	}
	return ""
}

func getNZBSegments(index int, file manifest.File, group *FileGroup) (int64, []storage.NZBSegment) {
	if len(file.Segments) == 0 {
		return 0, nil
	}

	sort.Slice(file.Segments, func(i, j int) bool {
		return file.Segments[i].Number < file.Segments[j].Number
	})

	// Segment numbers must form one contiguous range (usually 1..N, some
	// posters number from 0). A file with holes or duplicates cannot produce
	// a consistent offset map: the old code zero-filled missing slots, which
	// leaked segments with empty message-ids and offset 0 into the .meta
	// files, the streaming reader (non-monotonic offset table breaks its
	// binary search), and Download. Reject such files outright.
	minSegNum, ok := contiguousSegmentNumbers(file.Segments)
	if !ok {
		return 0, nil
	}

	sizing := group.segmentSizing(index, file)
	segmentGroup := ""
	if len(file.Groups) > 0 {
		segmentGroup = file.Groups[0]
	}

	nzbSegments := make([]storage.NZBSegment, len(file.Segments))
	var currentOffset int64
	for idx, segment := range file.Segments {
		// A segment without a message id can never be fetched; it would also
		// defeat the empty-slot duplicate check below.
		if segment.MessageID == "" {
			return 0, nil
		}
		segSize := sizing.sizeOf(idx, len(file.Segments), segment.Bytes)
		if segSize <= 0 {
			return 0, nil
		}

		// Normalize to the range base so 0- and 1-indexed numbering both map
		// onto a dense array. A duplicate number means the range check above
		// passed on count alone while another slot stays empty — reject.
		segIdx := segment.Number - minSegNum
		if nzbSegments[segIdx].MessageID != "" {
			return 0, nil
		}
		nzbSegments[segIdx] = storage.NZBSegment{
			Number:      segment.Number,
			MessageID:   segment.MessageID,
			Bytes:       segSize,
			StartOffset: currentOffset,
			EndOffset:   currentOffset + segSize - 1,
			Group:       segmentGroup,
		}
		currentOffset += segSize
	}
	return currentOffset, nzbSegments
}

// contiguousSegmentNumbers returns the lowest segment number and whether the
// count matches the number range.
func contiguousSegmentNumbers(segments []manifest.Segment) (int, bool) {
	minNum, maxNum := segments[0].Number, segments[0].Number
	for _, seg := range segments {
		minNum = min(minNum, seg.Number)
		maxNum = max(maxNum, seg.Number)
	}
	return minNum, maxNum-minNum+1 == len(segments)
}

// yencDecodedRatio estimates decoded size from encoded bytes (~3% yEnc overhead).
const yencDecodedRatio = 0.97

func estimateDecodedSize(encoded int64) int64 {
	return int64(float64(encoded) * yencDecodedRatio)
}

// segmentSizing holds the decoded sizes used to lay out one posted file.
type segmentSizing struct {
	segmentSize int64
	fileSize    int64
}

// segmentSizing prefers per-file yEnc metadata over the group's; the last
// file of a group uses the group's recorded last-file size.
func (f *FileGroup) segmentSizing(index int, file manifest.File) segmentSizing {
	metadata := *f.getMetadata()
	key := fileMetaKey(file)
	partMeta := f.fileMeta[key]
	sizing := segmentSizing{segmentSize: metadata.segmentSize, fileSize: metadata.fileSize}
	if partMeta.segmentSize > 0 {
		sizing.segmentSize = partMeta.segmentSize
	}
	if partMeta.fileSize > 0 {
		sizing.fileSize = partMeta.fileSize
		return sizing
	}
	isLegacyPositionalLast := metadata.lastFileKey == "" && index == len(f.Files)-1
	isRecordedLast := metadata.lastFileKey != "" && metadata.lastFileKey == key
	if isLegacyPositionalLast || isRecordedLast {
		sizing.fileSize = metadata.lastFileSize
	}
	return sizing
}

// sizeOf returns the decoded size of segment idx of count, falling back to an
// estimate from its encoded bytes.
func (s segmentSizing) sizeOf(idx, count int, encoded int64) int64 {
	size := s.segmentSize
	if idx == count-1 {
		size = s.lastSegmentSize(count, encoded)
	}
	if size <= 0 {
		size = estimateDecodedSize(encoded)
	}
	return size
}

// lastSegmentSize: the last segment may be smaller. When the file size is
// inconsistent with the segment count by more than 1.5 segments (metadata
// from a different file, e.g. mixed groups), estimate from encoded bytes.
func (s segmentSizing) lastSegmentSize(count int, encoded int64) int64 {
	fullSegsSize := s.segmentSize * int64(count-1)
	diff := s.fileSize - (fullSegsSize + s.segmentSize)
	if diff < 0 {
		diff = -diff
	}
	if diff > s.segmentSize*3/2 {
		return estimateDecodedSize(encoded)
	}
	return s.fileSize - fullSegsSize
}

func buildBaseSegments(group *FileGroup) ([]storage.NZBSegment, []storage.ArchiveVolumeInfo, error) {
	if len(group.Files) == 0 {
		return nil, nil, fmt.Errorf("archive group %s has no raw files", group.BaseName)
	}

	baseSegments := make([]storage.NZBSegment, 0)
	volumeInfos := make([]storage.ArchiveVolumeInfo, 0, len(group.Files))

	for idx, nzbFile := range group.Files {
		totalSize, segments := getNZBSegments(idx, nzbFile, group)
		if totalSize == 0 || len(segments) == 0 {
			return nil, nil, fmt.Errorf(
				"archive volume %q has incomplete or inconsistent segments",
				nzbFile.Filename,
			)
		}
		start := len(baseSegments)
		baseSegments = append(baseSegments, segments...)
		volumeInfos = append(volumeInfos, storage.ArchiveVolumeInfo{
			Name:         nzbFile.Filename,
			Size:         totalSize,
			SegmentStart: start,
			SegmentEnd:   len(baseSegments),
		})
	}

	return baseSegments, volumeInfos, nil
}

func buildArchiveVolumeDescriptors(group *FileGroup) ([]*types.Volume, error) {
	var volumes []*types.Volume

	if len(group.Files) == 0 {
		return nil, fmt.Errorf("archive group %s has no raw files", group.BaseName)
	}

	for idx, nzbFile := range group.Files {
		if len(nzbFile.Segments) == 0 {
			return nil, fmt.Errorf("archive volume %q has no segments", nzbFile.Filename)
		}

		totalSize, volumeSegments := getNZBSegments(idx, nzbFile, group)
		if totalSize == 0 || len(volumeSegments) == 0 {
			return nil, fmt.Errorf("archive volume %q has incomplete or inconsistent segments", nzbFile.Filename)
		}

		volumeName := nzbFile.Filename
		if volumeName == "" {
			volumeName = fmt.Sprintf("%s.part%03d", group.BaseName, idx+1)
		}

		volumes = append(volumes, &types.Volume{
			Index:    idx,
			Name:     volumeName,
			Size:     totalSize,
			Segments: volumeSegments,
		})
	}

	return volumes, nil
}

func buildExtractedArchiveFiles(
	group *FileGroup,
	password string,
	fileType storage.NZBFileType,
	baseSegments []storage.NZBSegment,
	volumeInfos []storage.ArchiveVolumeInfo,
	infos []*storage.ExtractedFileInfo,
) ([]*storage.NZBFile, error) {
	if len(baseSegments) == 0 {
		return nil, fmt.Errorf("archive has no base segments")
	}
	segmentIndex, err := newSegmentLayout(baseSegments)
	if err != nil {
		return nil, fmt.Errorf("index archive source segments: %w", err)
	}
	if validateVolumesErr := segmentIndex.validateVolumes(volumeInfos); validateVolumesErr != nil {
		return nil, fmt.Errorf("validate archive volume layout: %w", validateVolumesErr)
	}
	files := make([]*storage.NZBFile, 0, len(infos))

	for _, info := range infos {
		if info == nil || info.FileSize <= 0 {
			continue
		}
		if info.InternalPath == "" {
			info.InternalPath = NormalizeArchivePath(info.FileName)
		}
		name := info.FileName
		if name == "" {
			name = group.BaseName
		}
		name = utils.RemoveInvalidChars(name)

		// Pre-computed segments (from the RAR parser, etc.) take precedence.
		segments := info.Segments
		if len(segments) == 0 {
			if segments, err = sliceArchivedFile(segmentIndex, info); err != nil {
				return nil, err
			}
		}

		files = append(files, &storage.NZBFile{
			Name:         name,
			InternalPath: info.InternalPath,
			Groups:       getGroupsList(group.Groups),
			Segments:     segments,
			Password:     password,
			FileType:     fileType,
			Size:         info.FileSize,
			IsStored:     info.IsStored,
		})
	}

	return files, nil
}

// sliceArchivedFile maps a file's byte range onto the raw source segments.
func sliceArchivedFile(index *segmentLayout, info *storage.ExtractedFileInfo) ([]storage.NZBSegment, error) {
	if info.DataOffset <= 0 && info.FileSize <= 0 {
		return nil, fmt.Errorf("archived file %q has no source offset", info.InternalPath)
	}
	sliced, err := index.slice(info.DataOffset, info.FileSize, true)
	if err == nil && len(sliced) == 0 {
		err = fmt.Errorf("no source segments overlap the file range")
	}
	if err != nil {
		return nil, fmt.Errorf("map archived file %q to raw source: %w", info.InternalPath, err)
	}
	return sliced, nil
}

// NormalizeArchivePath cleans an archive member path into a slash-separated
// relative path ("" when nothing remains).
func NormalizeArchivePath(name string) string {
	trimmed := strings.TrimSpace(name)
	trimmed = strings.TrimLeft(trimmed, "./")
	if trimmed == "" {
		return ""
	}
	trimmed = strings.ReplaceAll(trimmed, "\\", "/")
	trimmed = path.Clean(trimmed)
	trimmed = strings.Trim(trimmed, "/")
	return trimmed
}
