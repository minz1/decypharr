package parser

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// ErrPrefixReadUnsupported asks the caller to use the full serving reader for
// transformations the analyzer deliberately does not duplicate.
var ErrPrefixReadUnsupported = errors.New("analyzer prefix read is unsupported")

// segmentsByOffset returns segments in ascending StartOffset order. Stored
// segments are already sorted and a head read only touches the first few, so
// cloning and sorting (a full copy of a remux's tens of thousands of
// segments, per 16KB head, for every media file in a repair sweep) only
// happens when the input really is out of order.
func segmentsByOffset(segments []storage.NZBSegment) []storage.NZBSegment {
	byOffset := func(left, right storage.NZBSegment) int {
		if order := cmp.Compare(left.StartOffset, right.StartOffset); order != 0 {
			return order
		}
		return cmp.Compare(left.Number, right.Number)
	}
	if slices.IsSortedFunc(segments, byOffset) {
		return segments
	}
	sorted := slices.Clone(segments)
	slices.SortFunc(sorted, byOffset)
	return sorted
}

// segmentCoversPosition validates a segment's range and reports whether it
// holds bytes at position; a segment starting past position is a gap.
func segmentCoversPosition(name string, segment storage.NZBSegment, position int64) (bool, error) {
	if segment.Bytes <= 0 {
		return false, fmt.Errorf("file %q has non-positive segment size %d", name, segment.Bytes)
	}
	segmentEnd := segment.StartOffset + segment.Bytes
	if segmentEnd < segment.StartOffset {
		return false, fmt.Errorf("file %q segment range overflows int64", name)
	}
	if segmentEnd <= position {
		return false, nil
	}
	if segment.StartOffset > position {
		return false, fmt.Errorf("file %q has a source gap at offset %d", name, position)
	}
	return true, nil
}

// copySegmentPrefix copies the segment's bytes from position into dst and
// returns how many were copied.
func (p *NZBParser) copySegmentPrefix(
	ctx context.Context,
	name string,
	segment storage.NZBSegment,
	dst []byte,
	position int64,
) (int64, error) {
	data, err := fetchSegmentData(ctx, p.source, segment)
	if err != nil {
		return 0, fmt.Errorf("read head segment %s for %q: %w", segment.MessageID, name, err)
	}
	within := position - segment.StartOffset
	count := min(int64(len(dst)), int64(len(data))-within)
	if count <= 0 {
		return 0, fmt.Errorf("file %q segment %s does not cover offset %d", name, segment.MessageID, position)
	}
	copy(dst[:count], data[within:within+count])
	return count, nil
}

// ReadFilePrefix reads a logical file head through the analyzer's shared body
// broker. Direct media and stored archive entries need no second NNTP fetch;
// encrypted or compressed entries remain on the serving reader's transform
// path.
func (p *NZBParser) ReadFilePrefix(ctx context.Context, file *storage.NZBFile, maxBytes int) ([]byte, error) {
	if p == nil || p.source == nil {
		return nil, fmt.Errorf("article source is nil")
	}
	if file == nil {
		return nil, fmt.Errorf("NZB file is nil")
	}
	if maxBytes <= 0 {
		return nil, nil
	}
	if file.IsEncrypted || (file.FileType != storage.NZBFileTypeMedia && !file.IsStored) {
		return nil, fmt.Errorf("%w for %q", ErrPrefixReadUnsupported, file.Name)
	}
	if len(file.Segments) == 0 {
		return nil, fmt.Errorf("file %q has no segments", file.Name)
	}

	wanted := int64(maxBytes)
	if file.Size > 0 {
		wanted = min(wanted, file.Size)
	}
	result := make([]byte, wanted)
	var position int64
	for _, segment := range segmentsByOffset(file.Segments) {
		if position == wanted {
			break
		}
		covers, err := segmentCoversPosition(file.Name, segment, position)
		if err != nil {
			return nil, err
		}
		if !covers {
			continue
		}
		count, err := p.copySegmentPrefix(ctx, file.Name, segment, result[position:], position)
		if err != nil {
			return nil, err
		}
		position += count
	}
	if position != wanted {
		return nil, fmt.Errorf("file %q head is only partially covered (%d of %d bytes)", file.Name, position, wanted)
	}
	return result, nil
}
