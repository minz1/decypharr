package usenet

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
	"unsafe"

	"github.com/klauspost/compress/zstd"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// On-disk NZB metadata format v2.
//
// Goals: small on disk, fast to decode, and low allocation pressure when
// loading. The format is split into three independently zstd-framed regions so
// that header-only reads (status/path/file-list, by far the most common access
// pattern) never decompress or allocate the multi-megabyte segment map:
//
//	[1]        magic byte (codecMagicV2)
//	[uvarint]  len(zstd(header))   + zstd(header)     -- NZB scalars + per-file meta (no segments)
//	[uvarint]  len(zstd(segMeta))  + zstd(segMeta)    -- columnar numeric/group columns for all segments
//	[...]      zstd(msgIDs)                            -- concatenated message-id bytes, length-prefixed
//
// Full decode produces a *storage.NZB whose segments live in ONE contiguous
// []NZBSegment backing array (each file takes a sub-slice, no per-file copy),
// whose Group strings are interned (one allocation per unique group), and whose
// MessageID strings alias the single decompressed msgIDs buffer via
// [unsafe.String] (one allocation for all ids instead of one per segment).
const codecMagicV2 = 0xB1

// errFileNotFound reports that an NZB has no live file with the given name.
var errFileNotFound = errors.New("file not found in NZB")

// nzbCodec owns the zstd state for v2 meta blobs. EncodeAll/DecodeAll on it
// are safe for concurrent use.
type nzbCodec struct {
	enc *zstd.Encoder
	dec *zstd.Decoder
}

// Encoder limits for small .meta blobs.
const (
	zstdEncoderConcurrency = 2
	zstdWindowSize         = 1 << 20
)

// newNZBCodec caps encoder concurrency and window: .meta blobs are small, so
// GOMAXPROCS encoder states each holding an 8MB history would be waste.
func newNZBCodec() (*nzbCodec, error) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(zstdEncoderConcurrency),
		zstd.WithWindowSize(zstdWindowSize),
	)
	if err != nil {
		return nil, fmt.Errorf("create zstd encoder: %w", err)
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	if err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("create zstd decoder: %w", err)
	}
	return &nzbCodec{enc: enc, dec: dec}, nil
}

// ---------------------------------------------------------------------------
// byte writer
// ---------------------------------------------------------------------------

type byteWriter struct {
	buf []byte
}

func (w *byteWriter) uvarint(v uint64) { w.buf = binary.AppendUvarint(w.buf, v) }
func (w *byteWriter) varint(v int64)   { w.buf = binary.AppendVarint(w.buf, v) }
func (w *byteWriter) str(s string) {
	w.uvarint(uint64(len(s)))
	w.buf = append(w.buf, s...)
}
func (w *byteWriter) raw(b []byte) {
	w.uvarint(uint64(len(b)))
	w.buf = append(w.buf, b...)
}
func (w *byteWriter) boolean(b bool) {
	if b {
		w.buf = append(w.buf, 1)
	} else {
		w.buf = append(w.buf, 0)
	}
}
func (w *byteWriter) f64(f float64) {
	w.buf = binary.LittleEndian.AppendUint64(w.buf, math.Float64bits(f))
}

// ---------------------------------------------------------------------------
// byte reader
// ---------------------------------------------------------------------------

// byteReader decodes the v2 primitives. Errors are sticky: after the first
// failure every read returns a zero value and err keeps that failure, so a
// decoder can read a whole record and check err once.
type byteReader struct {
	buf []byte
	pos int
	err error
}

func (r *byteReader) failf(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf("nzbcodec: "+format, args...)
	}
}

func (r *byteReader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.buf[r.pos:])
	if n <= 0 {
		r.failf("bad uvarint at %d", r.pos)
		return 0
	}
	r.pos += n
	return v
}

func (r *byteReader) varint() int64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Varint(r.buf[r.pos:])
	if n <= 0 {
		r.failf("bad varint at %d", r.pos)
		return 0
	}
	r.pos += n
	return v
}

// count reads an element count and rejects one larger than the bytes left,
// since every element occupies at least one byte. It bounds allocations sized
// from untrusted blobs.
func (r *byteReader) count() int {
	n := r.uvarint()
	if r.err != nil {
		return 0
	}
	remaining := len(r.buf) - r.pos
	if remaining < 0 || n > math.MaxInt32 || int(n) > remaining {
		r.failf("count %d exceeds %d remaining bytes at %d", n, remaining, r.pos)
		return 0
	}
	return int(n)
}

// segmentCount reads a per-file segment count, which sizes allocations in
// another region and is validated against it there.
func (r *byteReader) segmentCount() int {
	n := r.uvarint()
	if n > math.MaxInt32 {
		r.failf("segment count %d out of range", n)
		return 0
	}
	return int(n)
}

func (r *byteReader) span() []byte {
	n := r.count()
	if r.err != nil {
		return nil
	}
	b := r.buf[r.pos : r.pos+n]
	r.pos += n
	return b
}

// strCopy returns an owned copy (use for small/long-lived header strings).
func (r *byteReader) strCopy() string {
	return string(r.span())
}

// strAlias returns a string aliasing r.buf without copying. The caller must
// keep r.buf alive for as long as the returned string is used.
func (r *byteReader) strAlias() string {
	b := r.span()
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}

func (r *byteReader) bytesCopy() []byte {
	b := r.span()
	if len(b) == 0 {
		return nil
	}
	return bytes.Clone(b)
}

func (r *byteReader) boolean() bool {
	if r.err != nil {
		return false
	}
	if r.pos >= len(r.buf) {
		r.failf("bool out of range")
		return false
	}
	b := r.buf[r.pos]
	r.pos++
	return b != 0
}

func (r *byteReader) f64() float64 {
	if r.err != nil {
		return 0
	}
	if len(r.buf)-r.pos < float64Size {
		r.failf("float out of range")
		return 0
	}
	v := binary.LittleEndian.Uint64(r.buf[r.pos:])
	r.pos += float64Size
	return math.Float64frombits(v)
}

func (r *byteReader) strings() []string {
	n := r.count()
	if n == 0 {
		return nil
	}
	out := make([]string, n)
	for i := range out {
		// Group/header strings copied: few unique, long-lived.
		out[i] = r.strCopy()
	}
	return out
}

func (r *byteReader) time() time.Time {
	return time.Unix(r.varint(), 0)
}

// groupIndex reads a group-table index and checks it against the table.
func (r *byteReader) groupIndex(groups int) int {
	idx := r.uvarint()
	if r.err != nil {
		return 0
	}
	if idx > math.MaxInt32 {
		r.failf("group index %d out of range", idx)
		return 0
	}
	i := int(idx)
	if i >= groups {
		r.failf("group index %d out of range", i)
		return 0
	}
	return i
}

// skipVarints advances past n varints.
func (r *byteReader) skipVarints(n int) {
	for range n {
		r.varint()
	}
}

// float64Size is the encoded width of a float64.
const float64Size = 8

// ---------------------------------------------------------------------------
// encode
// ---------------------------------------------------------------------------

func (c *nzbCodec) encodeNZBV2(nzb *storage.NZB) []byte {
	header := encodeHeader(nzb)
	segMeta, msgIDs := encodeSegments(nzb)

	hc := c.enc.EncodeAll(header, nil)
	sc := c.enc.EncodeAll(segMeta, nil)
	mc := c.enc.EncodeAll(msgIDs, nil)

	out := make([]byte, 0, 1+binary.MaxVarintLen64*2+len(hc)+len(sc)+len(mc))
	out = append(out, codecMagicV2)
	out = binary.AppendUvarint(out, uint64(len(hc)))
	out = append(out, hc...)
	out = binary.AppendUvarint(out, uint64(len(sc)))
	out = append(out, sc...)
	out = append(out, mc...)
	return out
}

func encodeHeader(nzb *storage.NZB) []byte {
	w := &byteWriter{}
	w.str(nzb.ID)
	w.str(nzb.Name)
	w.str(nzb.Title)
	w.str(nzb.Path)
	w.varint(nzb.TotalSize)
	w.varint(nzb.DatePosted.Unix())
	w.str(nzb.Category)
	w.uvarint(uint64(len(nzb.Groups)))
	for _, g := range nzb.Groups {
		w.str(g)
	}
	w.boolean(nzb.Downloaded)
	w.varint(nzb.AddedOn.Unix())
	w.varint(nzb.LastActivity.Unix())
	w.str(nzb.Status)
	w.f64(nzb.Progress)
	w.f64(nzb.Percentage)
	w.varint(nzb.SizeDownloaded)
	w.varint(nzb.ETA)
	w.varint(nzb.Speed)
	w.varint(nzb.CompletedOn.Unix())
	w.boolean(nzb.IsBad)
	w.str(nzb.Storage)
	w.str(nzb.FailMessage)
	w.str(nzb.Password)

	w.uvarint(uint64(len(nzb.Files)))
	for i := range nzb.Files {
		f := &nzb.Files[i]
		// NzbID is omitted (filled from nzb.ID on decode).
		w.str(f.Name)
		w.str(f.InternalPath)
		w.varint(f.Size)
		w.varint(f.StartOffset)
		w.uvarint(uint64(len(f.Groups)))
		for _, g := range f.Groups {
			w.str(g)
		}
		w.str(string(f.FileType))
		w.str(f.Password)
		w.boolean(f.IsDeleted)
		w.boolean(f.IsStored)
		w.varint(f.SegmentSize)
		w.raw(f.EncryptionKey)
		w.raw(f.EncryptionIV)
		w.boolean(f.IsEncrypted)
		w.uvarint(uint64(len(f.Segments)))
	}
	return w.buf
}

// encodeSegments produces two buffers: segMeta (columnar numeric + group data
// for every segment across all files, in file order) and msgIDs (the
// concatenated, length-prefixed message ids).
func encodeSegments(nzb *storage.NZB) ([]byte, []byte) {
	var segments []*storage.NZBSegment
	for i := range nzb.Files {
		for j := range nzb.Files[i].Segments {
			segments = append(segments, &nzb.Files[i].Segments[j])
		}
	}

	// Group interning table and the per-segment index column.
	groupIdx := make(map[string]uint64)
	var groups []string
	idxCol := make([]uint64, len(segments))
	for i, seg := range segments {
		id, ok := groupIdx[seg.Group]
		if !ok {
			id = uint64(len(groups))
			groupIdx[seg.Group] = id
			groups = append(groups, seg.Group)
		}
		idxCol[i] = id
	}

	sw := &byteWriter{}
	sw.uvarint(uint64(len(groups)))
	for _, g := range groups {
		sw.str(g)
	}
	// Numeric columns (grouped by field for better compression).
	columns := []func(*storage.NZBSegment) int64{
		func(seg *storage.NZBSegment) int64 { return int64(seg.Number) },
		func(seg *storage.NZBSegment) int64 { return seg.Bytes },
		func(seg *storage.NZBSegment) int64 { return seg.StartOffset },
		func(seg *storage.NZBSegment) int64 { return seg.EndOffset },
		func(seg *storage.NZBSegment) int64 { return seg.SegmentDataStart },
	}
	for _, column := range columns {
		for _, seg := range segments {
			sw.varint(column(seg))
		}
	}
	for _, idx := range idxCol {
		sw.uvarint(idx)
	}

	// Message id region (its own buffer so a full decode retains only these
	// bytes, not the numeric columns).
	mw := &byteWriter{buf: make([]byte, 0, len(segments)*msgIDBytesHint)}
	for _, seg := range segments {
		mw.str(seg.MessageID)
	}
	return sw.buf, mw.buf
}

// msgIDBytesHint sizes the message-id buffer per segment.
const msgIDBytesHint = 48

// ---------------------------------------------------------------------------
// decode
// ---------------------------------------------------------------------------

// isCodecV2 reports whether data uses the v2 format (vs legacy protobuf).
func isCodecV2(data []byte) bool {
	return len(data) > 0 && data[0] == codecMagicV2
}

// splitRegions returns the three compressed regions of a v2 blob.
func splitRegions(data []byte) ([]byte, []byte, []byte, error) {
	if !isCodecV2(data) {
		return nil, nil, nil, fmt.Errorf("nzbcodec: not a v2 blob")
	}
	r := &byteReader{buf: data, pos: 1}
	hc, sc := r.span(), r.span()
	if r.err != nil {
		return nil, nil, nil, fmt.Errorf("split regions: %w", r.err)
	}
	return hc, sc, data[r.pos:], nil
}

// decodeNZBV2Header decodes only the NZB scalars and per-file metadata. The
// returned files have nil Segments. It never decompresses the segment regions.
func (c *nzbCodec) decodeNZBV2Header(data []byte) (*storage.NZB, error) {
	hc, _, _, err := splitRegions(data)
	if err != nil {
		return nil, err
	}
	header, err := c.dec.DecodeAll(hc, nil)
	if err != nil {
		return nil, fmt.Errorf("nzbcodec: decompress header: %w", err)
	}
	nzb, _, err := decodeHeader(header)
	return nzb, err
}

// decodeNZBV2 fully decodes an NZB including its segment map.
func (c *nzbCodec) decodeNZBV2(data []byte) (*storage.NZB, error) {
	hc, sc, mc, err := splitRegions(data)
	if err != nil {
		return nil, err
	}
	header, err := c.dec.DecodeAll(hc, nil)
	if err != nil {
		return nil, fmt.Errorf("nzbcodec: decompress header: %w", err)
	}
	nzb, counts, err := decodeHeader(header)
	if err != nil {
		return nil, err
	}

	segMeta, err := c.dec.DecodeAll(sc, nil)
	if err != nil {
		return nil, fmt.Errorf("nzbcodec: decompress seg meta: %w", err)
	}
	// msgIDs is retained (aliased by MessageID strings); keep this buffer alive.
	msgIDs, err := c.dec.DecodeAll(mc, nil)
	if err != nil {
		return nil, fmt.Errorf("nzbcodec: decompress msg ids: %w", err)
	}

	if decodeSegmentsErr := decodeSegments(nzb, counts, segMeta, msgIDs); decodeSegmentsErr != nil {
		return nil, decodeSegmentsErr
	}
	return nzb, nil
}

// decodeHeader returns the NZB (segments nil) and the per-file segment counts.
// Header strings are long-lived and few; they are copied so the (small)
// header buffer can be freed.
func decodeHeader(buf []byte) (*storage.NZB, []int, error) {
	r := &byteReader{buf: buf}
	nzb := &storage.NZB{}
	nzb.ID = r.strCopy()
	nzb.Name = r.strCopy()
	nzb.Title = r.strCopy()
	nzb.Path = r.strCopy()
	nzb.TotalSize = r.varint()
	nzb.DatePosted = r.time()
	nzb.Category = r.strCopy()
	nzb.Groups = r.strings()
	nzb.Downloaded = r.boolean()
	nzb.AddedOn = r.time()
	nzb.LastActivity = r.time()
	nzb.Status = r.strCopy()
	nzb.Progress = r.f64()
	nzb.Percentage = r.f64()
	nzb.SizeDownloaded = r.varint()
	nzb.ETA = r.varint()
	nzb.Speed = r.varint()
	nzb.CompletedOn = r.time()
	nzb.IsBad = r.boolean()
	nzb.Storage = r.strCopy()
	nzb.FailMessage = r.strCopy()
	nzb.Password = r.strCopy()

	nFiles := r.count()
	if r.err != nil {
		return nil, nil, r.err
	}
	nzb.Files = make([]storage.NZBFile, nFiles)
	counts := make([]int, nFiles)
	for i := range nzb.Files {
		counts[i] = decodeFileHeader(r, &nzb.Files[i])
		nzb.Files[i].NzbID = nzb.ID
	}
	if r.err != nil {
		return nil, nil, r.err
	}
	return nzb, counts, nil
}

// decodeFileHeader reads one file's metadata and returns its segment count.
// NzbID is not stored (filled from the NZB).
func decodeFileHeader(r *byteReader, f *storage.NZBFile) int {
	f.Name = r.strCopy()
	f.InternalPath = r.strCopy()
	f.Size = r.varint()
	f.StartOffset = r.varint()
	f.Groups = r.strings()
	f.FileType = storage.NZBFileType(r.strCopy())
	f.Password = r.strCopy()
	f.IsDeleted = r.boolean()
	f.IsStored = r.boolean()
	f.SegmentSize = r.varint()
	f.EncryptionKey = r.bytesCopy()
	f.EncryptionIV = r.bytesCopy()
	f.IsEncrypted = r.boolean()
	return r.segmentCount()
}

// segmentColumns assign the numeric columns, in encoded order.
func segmentColumns() []func(*storage.NZBSegment, int64) {
	return []func(*storage.NZBSegment, int64){
		func(seg *storage.NZBSegment, v int64) { seg.Number = int(v) },
		func(seg *storage.NZBSegment, v int64) { seg.Bytes = v },
		func(seg *storage.NZBSegment, v int64) { seg.StartOffset = v },
		func(seg *storage.NZBSegment, v int64) { seg.EndOffset = v },
		func(seg *storage.NZBSegment, v int64) { seg.SegmentDataStart = v },
	}
}

// decodeSegmentWindow fills segs from the columnar segMeta, where each column
// holds one value per segment of the whole NZB and segs covers
// [before, before+len(segs)) of them.
func decodeSegmentWindow(r *byteReader, segs []storage.NZBSegment, before, after int) {
	groups := r.strings()
	for _, assign := range segmentColumns() {
		r.skipVarints(before)
		for i := range segs {
			assign(&segs[i], r.varint())
		}
		r.skipVarints(after)
	}
	// Group column is last, so the trailing entries need no skip.
	for range before {
		r.uvarint()
	}
	for i := range segs {
		if idx := r.groupIndex(len(groups)); r.err == nil {
			segs[i].Group = groups[idx]
		}
	}
}

// decodeSegments fills nzb.Files[*].Segments from the columnar segMeta and the
// aliased msgIDs buffer. All segments share one backing array; each file takes
// a sub-slice. msgIDs must remain alive for the lifetime of the NZB.
func decodeSegments(nzb *storage.NZB, counts []int, segMeta, msgIDs []byte) error {
	total := 0
	for _, c := range counts {
		total += c
	}
	if err := checkSegmentTotal(total, segMeta); err != nil {
		return err
	}

	segs := make([]storage.NZBSegment, total)
	r := &byteReader{buf: segMeta}
	decodeSegmentWindow(r, segs, 0, 0)

	// Message ids alias the msgIDs buffer (no per-id allocation).
	mr := &byteReader{buf: msgIDs}
	for i := range segs {
		segs[i].MessageID = mr.strAlias()
	}
	if err := errors.Join(r.err, mr.err); err != nil {
		return err
	}

	// Hand out sub-slices to each file (no copy).
	off := 0
	for i := range nzb.Files {
		c := counts[i]
		nzb.Files[i].Segments = segs[off : off+c : off+c]
		off += c
	}
	return nil
}

// fileWindow locates the live (non-deleted) file named filename and the
// number of segments stored before it; ok is false when there is none.
func fileWindow(nzb *storage.NZB, counts []int, filename string) (int, int, bool) {
	before := 0
	for i := range nzb.Files {
		if nzb.Files[i].Name == filename && !nzb.Files[i].IsDeleted {
			return i, before, true
		}
		before += counts[i]
	}
	return 0, 0, false
}

// decodeFileV2 decodes the header plus exactly one file's segment map. It
// builds NZBSegment structs for that file alone and copies its message ids, so
// neither decompressed region stays reachable once it returns. Callers that
// need one file must use this instead of a full decode: decodeSegments builds
// every file's segments and aliases every id into the multi-megabyte message-id
// buffer, which keeps that buffer alive for as long as any id survives.
//
// It returns errFileNotFound when the file is absent or deleted.
func (c *nzbCodec) decodeFileV2(data []byte, filename string) (*storage.NZBFile, error) {
	hc, sc, mc, err := splitRegions(data)
	if err != nil {
		return nil, err
	}
	header, err := c.dec.DecodeAll(hc, nil)
	if err != nil {
		return nil, fmt.Errorf("nzbcodec: decompress header: %w", err)
	}
	nzb, counts, err := decodeHeader(header)
	if err != nil {
		return nil, err
	}
	target, before, found := fileWindow(nzb, counts, filename)
	if !found {
		return nil, errFileNotFound
	}

	file := nzb.Files[target]
	count := counts[target]
	if count == 0 {
		// Empty, not nil: matches what the full decode hands a zero-segment file.
		file.Segments = []storage.NZBSegment{}
		return &file, nil
	}
	total := 0
	for _, n := range counts {
		total += n
	}

	segMeta, err := c.dec.DecodeAll(sc, nil)
	if err != nil {
		return nil, fmt.Errorf("nzbcodec: decompress seg meta: %w", err)
	}
	if totalErr := checkSegmentTotal(total, segMeta); totalErr != nil {
		return nil, totalErr
	}
	segs := make([]storage.NZBSegment, count)
	r := &byteReader{buf: segMeta}
	decodeSegmentWindow(r, segs, before, total-before-count)
	if r.err != nil {
		return nil, r.err
	}

	msgIDs, err := c.dec.DecodeAll(mc, nil)
	if err != nil {
		return nil, fmt.Errorf("nzbcodec: decompress msg ids: %w", err)
	}
	mr := &byteReader{buf: msgIDs}
	for range before {
		mr.span()
	}
	for i := range segs {
		// Owned copies: lets the decompressed buffer be collected.
		segs[i].MessageID = mr.strCopy()
	}
	if mr.err != nil {
		return nil, mr.err
	}

	file.Segments = segs
	return &file, nil
}

// decodeFileMessageIDsSampled decodes only the sampled message ids of a single
// file. It decompresses just the header and the message-id region (never the
// numeric segMeta), builds no NZBSegment structs, and returns owned copies of
// only the sampled ids so the large decompressed buffer is freed immediately.
// This is the low-memory path used by repair availability probes.
//
// It returns (nil, nil) when the file is not found or has no segments.
func (c *nzbCodec) decodeFileMessageIDsSampled(data []byte, filename string, percent int) ([]string, error) {
	hc, _, mc, err := splitRegions(data)
	if err != nil {
		return nil, err
	}
	header, err := c.dec.DecodeAll(hc, nil)
	if err != nil {
		return nil, fmt.Errorf("nzbcodec: decompress header: %w", err)
	}
	nzb, counts, err := decodeHeader(header)
	if err != nil {
		return nil, err
	}
	target, before, found := fileWindow(nzb, counts, filename)
	if !found {
		return nil, nil
	}
	segCount := counts[target]
	if segCount == 0 {
		return nil, nil
	}

	msgIDs, err := c.dec.DecodeAll(mc, nil)
	if err != nil {
		return nil, fmt.Errorf("nzbcodec: decompress msg ids: %w", err)
	}
	// Every id has at least a one-byte length prefix.
	if before+segCount > len(msgIDs) {
		return nil, fmt.Errorf("nzbcodec: %d message ids cannot fit %d bytes", before+segCount, len(msgIDs))
	}

	want := sampleIndices(segCount, percent)
	wantSet := make(map[int]struct{}, len(want))
	for _, idx := range want {
		wantSet[idx] = struct{}{}
	}
	mr := &byteReader{buf: msgIDs}
	// Skip earlier files' ids without allocating.
	for range before {
		mr.span()
	}
	out := make([]string, 0, len(want))
	for j := range segCount {
		id := mr.span()
		if _, ok := wantSet[j]; ok {
			// Owned copy: lets the decompressed buffer be collected.
			out = append(out, string(id))
		}
	}
	if mr.err != nil {
		return nil, mr.err
	}
	return out, nil
}

// sampleIndices returns the segment indices to probe for availability: always
// the first and last, plus a uniform sample of the middle. Mirrors the
// distribution of sampleSegments but works on indices alone.
func sampleIndices(total, percent int) []int {
	if total == 0 {
		return nil
	}
	if percent >= 100 || total <= 3 {
		out := make([]int, total)
		for i := range out {
			out[i] = i
		}
		return out
	}

	targetCount := min(max((total*percent)/percentScale, endpointSamples), total)

	out := make([]int, 0, targetCount)
	out = append(out, 0)
	middleCount := targetCount - endpointSamples
	if middleCount > 0 {
		mlen := total - endpointSamples
		step := float64(mlen) / float64(middleCount+1)
		for i := range middleCount {
			idx := int(step * float64(i+1))
			if idx >= mlen {
				idx = mlen - 1
			}
			out = append(out, 1+idx)
		}
	}
	out = append(out, total-1)
	return out
}

// Sampling: percent is out of percentScale; the first and last segments
// (endpointSamples) are always probed.
const (
	percentScale    = 100
	endpointSamples = 2
)

// checkSegmentTotal rejects header segment counts the column region cannot
// hold (each segment stores at least six one-byte varints) before they size
// an allocation.
func checkSegmentTotal(total int, segMeta []byte) error {
	if total > len(segMeta) {
		return fmt.Errorf("nzbcodec: %d segments cannot fit %d column bytes", total, len(segMeta))
	}
	return nil
}
