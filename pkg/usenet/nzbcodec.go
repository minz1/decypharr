package usenet

import (
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

// newNZBCodec caps encoder concurrency and window: .meta blobs are small, so
// GOMAXPROCS encoder states each holding an 8MB history would be waste.
func newNZBCodec() (*nzbCodec, error) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(2),
		zstd.WithWindowSize(1<<20),
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

type byteReader struct {
	buf []byte
	pos int
}

func (r *byteReader) uvarint() (uint64, error) {
	v, n := binary.Uvarint(r.buf[r.pos:])
	if n <= 0 {
		return 0, fmt.Errorf("nzbcodec: bad uvarint at %d", r.pos)
	}
	r.pos += n
	return v, nil
}

func (r *byteReader) varint() (int64, error) {
	v, n := binary.Varint(r.buf[r.pos:])
	if n <= 0 {
		return 0, fmt.Errorf("nzbcodec: bad varint at %d", r.pos)
	}
	r.pos += n
	return v, nil
}

// count reads an element count and rejects one larger than the bytes left,
// since every element occupies at least one byte. It bounds allocations sized
// from untrusted blobs.
func (r *byteReader) count() (int, error) {
	n, err := r.uvarint()
	if err != nil {
		return 0, err
	}
	remaining := len(r.buf) - r.pos
	if remaining < 0 || n > math.MaxInt32 || int(n) > remaining {
		return 0, fmt.Errorf("nzbcodec: count %d exceeds %d remaining bytes at %d", n, remaining, r.pos)
	}
	return int(n), nil
}

func (r *byteReader) span() ([]byte, error) {
	n, err := r.count()
	if err != nil {
		return nil, err
	}
	b := r.buf[r.pos : r.pos+n]
	r.pos += n
	return b, nil
}

// strCopy returns an owned copy (use for small/long-lived header strings).
func (r *byteReader) strCopy() (string, error) {
	b, err := r.span()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// strAlias returns a string aliasing r.buf without copying. The caller must
// keep r.buf alive for as long as the returned string is used.
func (r *byteReader) strAlias() (string, error) {
	b, err := r.span()
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "", nil
	}
	return unsafe.String(&b[0], len(b)), nil
}

func (r *byteReader) bytesCopy() ([]byte, error) {
	b, err := r.span()
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

// skip advances past one length-prefixed span without materializing it.
func (r *byteReader) skip() error {
	_, err := r.span()
	return err
}

func (r *byteReader) boolean() (bool, error) {
	if r.pos >= len(r.buf) {
		return false, fmt.Errorf("nzbcodec: bool out of range")
	}
	b := r.buf[r.pos]
	r.pos++
	return b != 0, nil
}

func (r *byteReader) f64() (float64, error) {
	if r.pos+8 > len(r.buf) {
		return 0, fmt.Errorf("nzbcodec: float out of range")
	}
	v := binary.LittleEndian.Uint64(r.buf[r.pos:])
	r.pos += 8
	return math.Float64frombits(v), nil
}

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
func encodeSegments(nzb *storage.NZB) (segMeta, msgIDs []byte) {
	total := 0
	for i := range nzb.Files {
		total += len(nzb.Files[i].Segments)
	}

	mw := &byteWriter{buf: make([]byte, 0, total*48)}
	sw := &byteWriter{}

	// Group interning table.
	groupIdx := make(map[string]uint64)
	var groups []string
	idxOf := func(g string) uint64 {
		if id, ok := groupIdx[g]; ok {
			return id
		}
		id := uint64(len(groups))
		groupIdx[g] = id
		groups = append(groups, g)
		return id
	}

	// Pre-walk to build the group table and per-segment index column data.
	idxCol := make([]uint64, 0, total)
	for i := range nzb.Files {
		for j := range nzb.Files[i].Segments {
			idxCol = append(idxCol, idxOf(nzb.Files[i].Segments[j].Group))
		}
	}

	// Group table.
	sw.uvarint(uint64(len(groups)))
	for _, g := range groups {
		sw.str(g)
	}

	// Numeric columns (grouped by field for better compression).
	for i := range nzb.Files {
		for j := range nzb.Files[i].Segments {
			sw.varint(int64(nzb.Files[i].Segments[j].Number))
		}
	}
	for i := range nzb.Files {
		for j := range nzb.Files[i].Segments {
			sw.varint(nzb.Files[i].Segments[j].Bytes)
		}
	}
	for i := range nzb.Files {
		for j := range nzb.Files[i].Segments {
			sw.varint(nzb.Files[i].Segments[j].StartOffset)
		}
	}
	for i := range nzb.Files {
		for j := range nzb.Files[i].Segments {
			sw.varint(nzb.Files[i].Segments[j].EndOffset)
		}
	}
	for i := range nzb.Files {
		for j := range nzb.Files[i].Segments {
			sw.varint(nzb.Files[i].Segments[j].SegmentDataStart)
		}
	}
	for _, idx := range idxCol {
		sw.uvarint(idx)
	}

	// Message id region (its own buffer so a full decode retains only these
	// bytes, not the numeric columns).
	for i := range nzb.Files {
		for j := range nzb.Files[i].Segments {
			mw.str(nzb.Files[i].Segments[j].MessageID)
		}
	}

	return sw.buf, mw.buf
}

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
	hc, err := r.span()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("nzbcodec: header region: %w", err)
	}
	sc, err := r.span()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("nzbcodec: seg region: %w", err)
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
func decodeHeader(buf []byte) (*storage.NZB, []int, error) {
	r := &byteReader{buf: buf}
	nzb := &storage.NZB{}

	var err error
	get := func(dst *string) bool {
		var s string
		if s, err = r.strCopy(); err != nil {
			return false
		}
		*dst = s
		return true
	}

	// Header strings are long-lived and few; copy them so the (small) header
	// buffer can be freed.
	if !get(&nzb.ID) || !get(&nzb.Name) || !get(&nzb.Title) || !get(&nzb.Path) {
		return nil, nil, err
	}
	if nzb.TotalSize, err = r.varint(); err != nil {
		return nil, nil, err
	}
	if nzb.DatePosted, err = readTime(r); err != nil {
		return nil, nil, err
	}
	if !get(&nzb.Category) {
		return nil, nil, err
	}
	if nzb.Groups, err = readStrings(r); err != nil {
		return nil, nil, err
	}
	if nzb.Downloaded, err = r.boolean(); err != nil {
		return nil, nil, err
	}
	if nzb.AddedOn, err = readTime(r); err != nil {
		return nil, nil, err
	}
	if nzb.LastActivity, err = readTime(r); err != nil {
		return nil, nil, err
	}
	if !get(&nzb.Status) {
		return nil, nil, err
	}
	if nzb.Progress, err = r.f64(); err != nil {
		return nil, nil, err
	}
	if nzb.Percentage, err = r.f64(); err != nil {
		return nil, nil, err
	}
	if nzb.SizeDownloaded, err = r.varint(); err != nil {
		return nil, nil, err
	}
	if nzb.ETA, err = r.varint(); err != nil {
		return nil, nil, err
	}
	if nzb.Speed, err = r.varint(); err != nil {
		return nil, nil, err
	}
	if nzb.CompletedOn, err = readTime(r); err != nil {
		return nil, nil, err
	}
	if nzb.IsBad, err = r.boolean(); err != nil {
		return nil, nil, err
	}
	if !get(&nzb.Storage) || !get(&nzb.FailMessage) || !get(&nzb.Password) {
		return nil, nil, err
	}

	nFiles, err := r.count()
	if err != nil {
		return nil, nil, err
	}
	nzb.Files = make([]storage.NZBFile, nFiles)
	counts := make([]int, nFiles)
	for i := range nFiles {
		f := &nzb.Files[i]
		f.NzbID = nzb.ID
		var ft string
		if !get(&f.Name) || !get(&f.InternalPath) {
			return nil, nil, err
		}
		if f.Size, err = r.varint(); err != nil {
			return nil, nil, err
		}
		if f.StartOffset, err = r.varint(); err != nil {
			return nil, nil, err
		}
		if f.Groups, err = readStrings(r); err != nil {
			return nil, nil, err
		}
		if !get(&ft) {
			return nil, nil, err
		}
		f.FileType = storage.NZBFileType(ft)
		if !get(&f.Password) {
			return nil, nil, err
		}
		if f.IsDeleted, err = r.boolean(); err != nil {
			return nil, nil, err
		}
		if f.IsStored, err = r.boolean(); err != nil {
			return nil, nil, err
		}
		if f.SegmentSize, err = r.varint(); err != nil {
			return nil, nil, err
		}
		if f.EncryptionKey, err = r.bytesCopy(); err != nil {
			return nil, nil, err
		}
		if f.EncryptionIV, err = r.bytesCopy(); err != nil {
			return nil, nil, err
		}
		if f.IsEncrypted, err = r.boolean(); err != nil {
			return nil, nil, err
		}
		c, uvarintErr := r.uvarint()
		if uvarintErr != nil {
			return nil, nil, uvarintErr
		}
		if c > math.MaxInt32 {
			return nil, nil, fmt.Errorf("nzbcodec: file %d segment count %d out of range", i, c)
		}
		counts[i] = int(c)
	}
	return nzb, counts, nil
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
	r := &byteReader{buf: segMeta}

	// Group table.
	groups, err := readStrings(r)
	if err != nil {
		return err
	}

	segs := make([]storage.NZBSegment, total)

	for i := range total {
		v, varintErr := r.varint()
		if varintErr != nil {
			return varintErr
		}
		segs[i].Number = int(v)
	}
	for i := range total {
		if segs[i].Bytes, err = r.varint(); err != nil {
			return err
		}
	}
	for i := range total {
		if segs[i].StartOffset, err = r.varint(); err != nil {
			return err
		}
	}
	for i := range total {
		if segs[i].EndOffset, err = r.varint(); err != nil {
			return err
		}
	}
	for i := range total {
		if segs[i].SegmentDataStart, err = r.varint(); err != nil {
			return err
		}
	}
	for i := range total {
		idx, uvarintErr := r.uvarint()
		if uvarintErr != nil {
			return uvarintErr
		}
		if idx >= uint64(len(groups)) {
			return fmt.Errorf("nzbcodec: group index %d out of range", idx)
		}
		segs[i].Group = groups[idx]
	}

	// Message ids alias the msgIDs buffer (no per-id allocation).
	mr := &byteReader{buf: msgIDs}
	for i := range total {
		if segs[i].MessageID, err = mr.strAlias(); err != nil {
			return err
		}
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

	// Locate the requested (non-deleted) file and its segment range.
	target := -1
	before := 0
	for i := range nzb.Files {
		if nzb.Files[i].Name == filename && !nzb.Files[i].IsDeleted {
			target = i
			break
		}
		before += counts[i]
	}
	if target == -1 {
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
	for _, c := range counts {
		total += c
	}
	after := total - before - count

	segMeta, err := c.dec.DecodeAll(sc, nil)
	if err != nil {
		return nil, fmt.Errorf("nzbcodec: decompress seg meta: %w", err)
	}
	if totalErr := checkSegmentTotal(total, segMeta); totalErr != nil {
		return nil, totalErr
	}
	r := &byteReader{buf: segMeta}
	groups, err := readStrings(r)
	if err != nil {
		return nil, err
	}

	segs := make([]storage.NZBSegment, count)

	// The numeric columns each hold one value per segment of the whole NZB, so
	// reaching this file's window means reading past the files before it.
	column := func(assign func(seg *storage.NZBSegment, v int64)) error {
		for range before {
			if _, varintErr := r.varint(); varintErr != nil {
				return varintErr
			}
		}
		for i := range segs {
			v, varintErr := r.varint()
			if varintErr != nil {
				return varintErr
			}
			assign(&segs[i], v)
		}
		for range after {
			if _, varintErr := r.varint(); varintErr != nil {
				return varintErr
			}
		}
		return nil
	}

	if columnErr := column(func(seg *storage.NZBSegment, v int64) { seg.Number = int(v) }); columnErr != nil {
		return nil, columnErr
	}
	if columnErr := column(func(seg *storage.NZBSegment, v int64) { seg.Bytes = v }); columnErr != nil {
		return nil, columnErr
	}
	if columnErr := column(func(seg *storage.NZBSegment, v int64) { seg.StartOffset = v }); columnErr != nil {
		return nil, columnErr
	}
	if columnErr := column(func(seg *storage.NZBSegment, v int64) { seg.EndOffset = v }); columnErr != nil {
		return nil, columnErr
	}
	if columnErr := column(func(seg *storage.NZBSegment, v int64) { seg.SegmentDataStart = v }); columnErr != nil {
		return nil, columnErr
	}

	// Group column is last, so the trailing entries need no skip.
	for range before {
		if _, uvarintErr := r.uvarint(); uvarintErr != nil {
			return nil, uvarintErr
		}
	}
	for i := range segs {
		idx, uvarintErr := r.uvarint()
		if uvarintErr != nil {
			return nil, uvarintErr
		}
		if idx >= uint64(len(groups)) {
			return nil, fmt.Errorf("nzbcodec: group index %d out of range", idx)
		}
		segs[i].Group = groups[idx]
	}

	msgIDs, err := c.dec.DecodeAll(mc, nil)
	if err != nil {
		return nil, fmt.Errorf("nzbcodec: decompress msg ids: %w", err)
	}
	mr := &byteReader{buf: msgIDs}
	for range before {
		if skipErr := mr.skip(); skipErr != nil {
			return nil, skipErr
		}
	}
	for i := range segs {
		// Owned copies: lets the decompressed buffer be collected.
		if segs[i].MessageID, err = mr.strCopy(); err != nil {
			return nil, err
		}
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
// It returns (nil, -1, nil) when the file is not found or has no segments.
func (c *nzbCodec) decodeFileMessageIDsSampled(data []byte, filename string, percent int) ([]string, int, error) {
	hc, _, mc, err := splitRegions(data)
	if err != nil {
		return nil, 0, err
	}
	header, err := c.dec.DecodeAll(hc, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("nzbcodec: decompress header: %w", err)
	}
	nzb, counts, err := decodeHeader(header)
	if err != nil {
		return nil, 0, err
	}

	// Locate the requested (non-deleted) file and its segment range.
	target := -1
	before := 0
	for i := range nzb.Files {
		if nzb.Files[i].Name == filename && !nzb.Files[i].IsDeleted {
			target = i
			break
		}
		before += counts[i]
	}
	if target == -1 {
		return nil, -1, nil
	}
	segCount := counts[target]
	if segCount == 0 {
		return nil, 0, nil
	}

	msgIDs, err := c.dec.DecodeAll(mc, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("nzbcodec: decompress msg ids: %w", err)
	}
	// Every id has at least a one-byte length prefix.
	if before+segCount > len(msgIDs) {
		return nil, 0, fmt.Errorf("nzbcodec: %d message ids cannot fit %d bytes", before+segCount, len(msgIDs))
	}

	want := sampleIndices(segCount, percent)
	wantSet := make(map[int]struct{}, len(want))
	for _, idx := range want {
		wantSet[idx] = struct{}{}
	}
	mr := &byteReader{buf: msgIDs}

	// Skip earlier files' ids without allocating.
	for range before {
		if skipErr := mr.skip(); skipErr != nil {
			return nil, 0, skipErr
		}
	}

	out := make([]string, 0, len(want))
	for j := range segCount {
		if _, ok := wantSet[j]; ok {
			// Owned copy: lets the decompressed buffer be collected.
			s, strCopyErr := mr.strCopy()
			if strCopyErr != nil {
				return nil, 0, strCopyErr
			}
			out = append(out, s)
			continue
		}
		if skipErr := mr.skip(); skipErr != nil {
			return nil, 0, skipErr
		}
	}
	return out, segCount, nil
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

	targetCount := min(max((total*percent)/100, 2), total)

	out := make([]int, 0, targetCount)
	out = append(out, 0)
	middleCount := targetCount - 2
	if middleCount > 0 {
		mlen := total - 2
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

func readStrings(r *byteReader) ([]string, error) {
	n, err := r.count()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	out := make([]string, n)
	for i := range out {
		// Group/header strings copied: few unique, long-lived.
		if out[i], err = r.strCopy(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// checkSegmentTotal rejects header segment counts the column region cannot
// hold (each segment stores at least six one-byte varints) before they size
// an allocation.
func checkSegmentTotal(total int, segMeta []byte) error {
	if total > len(segMeta) {
		return fmt.Errorf("nzbcodec: %d segments cannot fit %d column bytes", total, len(segMeta))
	}
	return nil
}

func readTime(r *byteReader) (time.Time, error) {
	sec, err := r.varint()
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, 0), nil
}
