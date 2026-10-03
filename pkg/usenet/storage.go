package usenet

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/sourcegraph/conc/pool"
	"google.golang.org/protobuf/proto"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

const (
	metaFileExtension = ".meta"
	metaDirName       = "meta"
	// metaMigrationMarker is written to the meta dir once all legacy proto
	// files have been upgraded to the v2 codec, so migration runs at most once.
	metaMigrationMarker = ".codec-v2.done"
)

// migrationWorkers bounds concurrent legacy re-encodes.
const migrationWorkers = 6

// metaFileMode keeps NZB metadata (passwords, keys) private to the service.
const metaFileMode os.FileMode = 0o600

const (
	NZBStatusPending     = "pending"
	NZBStatusParsing     = "parsing"
	NZBStatusDownloading = "downloading"
	NZBStatusCompleted   = "completed"
	NZBStatusFailed      = "failed"
)

// NZBStorage handles file-based persistence of NZB metadata using protobuf.
type NZBStorage struct {
	metaDir string
	codec   *nzbCodec
	logger  zerolog.Logger
	mu      sync.RWMutex // Protects file operations and cached stats

	// Cached stats for fast Stats() reads without filesystem scans.
	metaCount      int
	metaTotalBytes int64
}

// NewNZBStorage creates a file-based NZB storage in metaDir.
func NewNZBStorage(metaDir string, log zerolog.Logger) (*NZBStorage, error) {
	if err := os.MkdirAll(metaDir, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create meta directory: %w", err)
	}

	codec, err := newNZBCodec()
	if err != nil {
		return nil, err
	}
	s := &NZBStorage{
		metaDir: metaDir,
		codec:   codec,
		logger:  log,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if statsErr := s.recalculateStatsLocked(); statsErr != nil {
		return nil, fmt.Errorf("failed to initialize NZB stats cache: %w", statsErr)
	}

	return s, nil
}

// metaFilePath returns the path for a given NZB ID.
func (s *NZBStorage) metaFilePath(id string) string {
	return filepath.Join(s.metaDir, id+metaFileExtension)
}

// recalculateStatsLocked rebuilds cached stats by scanning metadata files.
// Caller must hold s.mu.
func (s *NZBStorage) recalculateStatsLocked() error {
	entries, err := os.ReadDir(s.metaDir)
	if err != nil {
		return fmt.Errorf("failed to read meta directory: %w", err)
	}

	count := 0
	var totalSize int64
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != metaFileExtension {
			continue
		}
		count++
		info, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("failed to stat meta file %s: %w", entry.Name(), infoErr)
		}
		totalSize += info.Size()
	}

	s.metaCount = count
	s.metaTotalBytes = totalSize
	return nil
}

// AddNZB saves an NZB to file storage.
func (s *NZBStorage) AddNZB(nzb *storage.NZB) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeNZBLocked(nzb)
}

// writeNZBLocked persists an NZB and updates cached storage stats. Caller must
// hold s.mu.
func (s *NZBStorage) writeNZBLocked(nzb *storage.NZB) error {
	if nzb == nil || strings.TrimSpace(nzb.ID) == "" {
		return fmt.Errorf("NZB and ID are required")
	}

	data := s.codec.encodeNZBV2(nzb)

	path := s.metaFilePath(nzb.ID)
	var oldSize int64
	alreadyExists := false
	if info, statErr := os.Stat(path); statErr == nil {
		alreadyExists = true
		oldSize = info.Size()
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("failed to stat existing NZB meta file: %w", statErr)
	}

	// Write atomically using temp file
	tmpPath := path + ".tmp"
	if writeFileErr := os.WriteFile(tmpPath, data, metaFileMode); writeFileErr != nil {
		return fmt.Errorf("failed to write NZB meta file: %w", writeFileErr)
	}

	if renameErr := os.Rename(tmpPath, path); renameErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to rename NZB meta file: %w", renameErr)
	}

	newSize := int64(len(data))
	if alreadyExists {
		s.metaTotalBytes += newSize - oldSize
	} else {
		s.metaCount++
		s.metaTotalBytes += newSize
	}

	return nil
}

// GetNZB retrieves an NZB from file storage.
func (s *NZBStorage) GetNZB(id string) (*storage.NZB, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := s.metaFilePath(id)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("nzb not found: %s", id)
		}
		return nil, fmt.Errorf("failed to read NZB meta file: %w", err)
	}

	return s.codec.decodeNZB(data)
}

// GetNZBHeader retrieves an NZB without its segment map. It is far cheaper than
// GetNZB for the common case of only needing scalar/file metadata (status,
// path, file list). For legacy proto files it falls back to a full decode.
func (s *NZBStorage) GetNZBHeader(id string) (*storage.NZB, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := s.metaFilePath(id)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("nzb not found: %s", id)
		}
		return nil, fmt.Errorf("failed to read NZB meta file: %w", err)
	}

	if isCodecV2(data) {
		return s.codec.decodeNZBV2Header(data)
	}
	return s.codec.decodeNZB(data)
}

// GetNZBFile returns one file of an NZB with its segment map. It decodes only
// the requested file, so probing one file of a large NZB neither builds nor
// retains the segment maps of every other file - the full decode aliases every
// message id into one large buffer, which then stays alive for as long as any
// of those ids does. Legacy proto files fall back to a full decode. A missing
// or deleted file yields errFileNotFound.
func (s *NZBStorage) GetNZBFile(id, filename string) (*storage.NZBFile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := s.metaFilePath(id)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("nzb not found: %s", id)
		}
		return nil, fmt.Errorf("failed to read NZB meta file: %w", err)
	}

	if isCodecV2(data) {
		return s.codec.decodeFileV2(data, filename)
	}

	nzb, err := s.codec.decodeNZB(data)
	if err != nil {
		return nil, err
	}
	for i := range nzb.Files {
		if nzb.Files[i].Name == filename && !nzb.Files[i].IsDeleted {
			file := nzb.Files[i]
			return &file, nil
		}
	}
	return nil, errFileNotFound
}

// SampleFileMessageIDs returns the sampled message ids for a single file,
// used by availability/repair probes. For v2 blobs it decodes only that file's
// sampled ids (no numeric columns, no NZBSegment allocation, no other files),
// which keeps repair sweeps from holding full segment maps in memory. Legacy
// proto files fall back to a full decode. A nil slice with nil error means the
// file was not found or has no segments.
func (s *NZBStorage) SampleFileMessageIDs(id, filename string, percent int) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := s.metaFilePath(id)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("nzb not found: %s", id)
		}
		return nil, fmt.Errorf("failed to read NZB meta file: %w", err)
	}

	if isCodecV2(data) {
		return s.codec.decodeFileMessageIDsSampled(data, filename, percent)
	}

	// Legacy proto: full decode then sample in memory.
	nzb, err := s.codec.decodeNZB(data)
	if err != nil {
		return nil, err
	}
	f := nzb.GetFileByName(filename)
	if f == nil || len(f.Segments) == 0 {
		return nil, nil
	}
	want := sampleIndices(len(f.Segments), percent)
	ids := make([]string, 0, len(want))
	for _, idx := range want {
		ids = append(ids, f.Segments[idx].MessageID)
	}
	return ids, nil
}

// decodeNZB decodes a meta blob, supporting both the v2 codec and legacy
// protobuf files (which migrate to v2 on their next write).
func (c *nzbCodec) decodeNZB(data []byte) (*storage.NZB, error) {
	if isCodecV2(data) {
		return c.decodeNZBV2(data)
	}

	var pb NZBProto
	if err := proto.Unmarshal(data, &pb); err != nil {
		return nil, fmt.Errorf("failed to unmarshal NZB: %w", err)
	}
	return protoToNZB(&pb), nil
}

// DeleteNZB removes an NZB from file storage.
func (s *NZBStorage) DeleteNZB(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.metaFilePath(id)
	var oldSize int64
	alreadyExists := false
	if info, statErr := os.Stat(path); statErr == nil {
		alreadyExists = true
		oldSize = info.Size()
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("failed to stat NZB meta file before delete: %w", statErr)
	}

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete NZB meta file: %w", err)
	}

	if alreadyExists {
		if s.metaCount > 0 {
			s.metaCount--
		}
		s.metaTotalBytes -= oldSize
		if s.metaTotalBytes < 0 {
			s.metaTotalBytes = 0
		}
	}

	return nil
}

// ForEachNZB iterates over all NZBs in storage.
func (s *NZBStorage) ForEachNZB(fn func(*storage.NZB) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.metaDir)
	if err != nil {
		return fmt.Errorf("failed to read meta directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != metaFileExtension {
			continue
		}

		path := filepath.Join(s.metaDir, entry.Name())
		data, readFileErr := os.ReadFile(path)
		if readFileErr != nil {
			s.logger.Warn().Err(readFileErr).Str("file", entry.Name()).Msg("Failed to read NZB meta file")
			continue
		}

		nzb, readFileErr := s.codec.decodeNZB(data)
		if readFileErr != nil {
			s.logger.Warn().Err(readFileErr).Str("file", entry.Name()).Msg("Failed to decode NZB")
			continue
		}

		if fnErr := fn(nzb); fnErr != nil {
			return fnErr
		}
	}

	return nil
}

// GetAllNZBIDs returns all NZB IDs in storage.
func (s *NZBStorage) GetAllNZBIDs() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.metaDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read meta directory: %w", err)
	}

	var ids []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != metaFileExtension {
			continue
		}
		// Extract ID from filename (remove .meta extension)
		id := entry.Name()[:len(entry.Name())-len(metaFileExtension)]
		ids = append(ids, id)
	}

	return ids, nil
}

// Exists checks if an NZB exists in storage.
func (s *NZBStorage) Exists(id string) bool {
	path := s.metaFilePath(id)
	_, err := os.Stat(path)
	return err == nil
}

// Count returns the number of NZBs in storage.
func (s *NZBStorage) Count() (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.metaCount, nil
}

// Stats returns storage statistics.
func (s *NZBStorage) Stats() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return map[string]any{
		"count":       s.metaCount,
		"total_bytes": s.metaTotalBytes,
		"meta_dir":    s.metaDir,
	}
}

// MigrateLegacy rewrites any legacy protobuf .meta files to the v2 codec,
// reclaiming the ~4x size difference for NZBs that aren't otherwise re-saved.
//
// It runs at most once: a marker file is written after a clean pass, so
// subsequent calls (e.g. every restart) return immediately without scanning the
// directory. The heavy decode/encode work runs lock-free across a small worker
// pool; the storage lock is taken only briefly per file for a re-check + atomic
// rename, so a multi-thousand-file migration neither blocks startup nor starves
// concurrent readers. Each rewrite uses temp-file + atomic rename, so readers
// always observe a fully-decodable file (old proto or new v2). Decode failures
// are logged and skipped rather than aborting. Returns the number migrated.
func (s *NZBStorage) MigrateLegacy() (int, error) {
	if s.migrationMarkerExists() {
		return 0, nil
	}

	legacy, err := s.legacyMetaFiles()
	if err != nil {
		return 0, err
	}

	if len(legacy) == 0 {
		s.writeMigrationMarker()
		return 0, nil
	}

	s.logger.Info().Int("legacy", len(legacy)).Msg("Migration: upgrading legacy NZB meta to v2")

	var migrated, failed atomic.Int64
	pl := pool.New().WithMaxGoroutines(min(runtime.NumCPU(), migrationWorkers))

	for _, path := range legacy {
		pl.Go(func() {
			ok, migrateFileErr := s.migrateFile(path)
			if migrateFileErr != nil {
				s.logger.Warn().
					Err(migrateFileErr).
					Str("file", filepath.Base(path)).
					Msg("Migration: failed to migrate file")
				failed.Add(1)
				return
			}
			if ok {
				if n := migrated.Add(1); n%1000 == 0 {
					s.logger.Info().Int64("migrated", n).Int("total", len(legacy)).Msg("Migration: progress")
				}
			}
		})
	}

	pl.Wait()

	// Recompute cached stats once from disk rather than racing per-file deltas.
	s.mu.Lock()
	_ = s.recalculateStatsLocked()
	s.mu.Unlock()

	if failed.Load() == 0 {
		s.writeMigrationMarker()
	}
	s.logger.Info().
		Int64("migrated", migrated.Load()).
		Int64("failed", failed.Load()).
		Msg("Migration: completed legacy NZB meta upgrade")
	return int(migrated.Load()), nil
}

// legacyMetaFiles lists the meta files not yet in the v2 codec, using a cheap
// lock-free first-byte probe.
func (s *NZBStorage) legacyMetaFiles() ([]string, error) {
	s.mu.RLock()
	entries, err := os.ReadDir(s.metaDir)
	s.mu.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("failed to read meta directory: %w", err)
	}
	var legacy []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != metaFileExtension {
			continue
		}
		path := filepath.Join(s.metaDir, entry.Name())
		v2, probeErr := fileIsCodecV2(path)
		if probeErr != nil {
			s.logger.Warn().Err(probeErr).Str("file", entry.Name()).Msg("Migration: failed to probe file")
			continue
		}
		if !v2 {
			legacy = append(legacy, path)
		}
	}
	return legacy, nil
}

// migrateFile re-encodes one legacy proto meta file to v2. The expensive
// read/decode/encode runs lock-free; the storage lock is held only for the
// final re-check + atomic rename so a concurrent AddNZB can't be clobbered
// (AddNZB always writes v2, so a file that became v2 meanwhile is skipped).
// File operations go through an [os.Root] so a directory entry name can never
// address anything outside the meta directory.
func (s *NZBStorage) migrateFile(path string) (bool, error) {
	root, err := os.OpenRoot(s.metaDir)
	if err != nil {
		return false, fmt.Errorf("open meta dir: %w", err)
	}
	defer root.Close()
	name := filepath.Base(path)

	data, err := root.ReadFile(name)
	if err != nil {
		return false, fmt.Errorf("read: %w", err)
	}
	if isCodecV2(data) {
		return false, nil
	}

	nzb, err := s.codec.decodeNZB(data)
	if err != nil {
		return false, fmt.Errorf("decode: %w", err)
	}
	out := s.codec.encodeNZBV2(nzb)

	// Unique temp name so it can't collide with AddNZB's "<path>.tmp".
	tmpName := name + ".v2tmp"
	if writeFileErr := root.WriteFile(tmpName, out, metaFileMode); writeFileErr != nil {
		return false, fmt.Errorf("write temp: %w", writeFileErr)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// If AddNZB rewrote this file as v2 while we were encoding, its content is
	// newer — don't overwrite it with our re-encoded older copy.
	if cur, cerr := fileIsCodecV2(path); cerr == nil && cur {
		_ = root.Remove(tmpName)
		return false, nil
	}
	if renameErr := root.Rename(tmpName, name); renameErr != nil {
		_ = root.Remove(tmpName)
		return false, fmt.Errorf("rename: %w", renameErr)
	}
	return true, nil
}

func (s *NZBStorage) migrationMarkerPath() string {
	return filepath.Join(s.metaDir, metaMigrationMarker)
}

func (s *NZBStorage) migrationMarkerExists() bool {
	_, err := os.Stat(s.migrationMarkerPath())
	return err == nil
}

func (s *NZBStorage) writeMigrationMarker() {
	if err := os.WriteFile(s.migrationMarkerPath(), []byte("v2\n"), metaFileMode); err != nil {
		s.logger.Warn().Err(err).Msg("Migration: failed to write completion marker")
	}
}

// fileIsCodecV2 cheaply reports whether a meta file already uses the v2 codec
// by reading only its first byte.
func fileIsCodecV2(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	var b [1]byte
	n, err := f.Read(b[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return n == 1 && b[0] == codecMagicV2, nil
}

func protoToNZB(pb *NZBProto) *storage.NZB {
	nzb := &storage.NZB{
		ID:             pb.GetId(),
		Name:           pb.GetName(),
		Title:          pb.GetTitle(),
		Path:           pb.GetPath(),
		TotalSize:      pb.GetTotalSize(),
		DatePosted:     time.Unix(pb.GetDatePostedUnix(), 0),
		Category:       pb.GetCategory(),
		Groups:         pb.GetGroups(),
		Downloaded:     pb.GetDownloaded(),
		AddedOn:        time.Unix(pb.GetAddedOnUnix(), 0),
		LastActivity:   time.Unix(pb.GetLastActivityUnix(), 0),
		Status:         pb.GetStatus(),
		Progress:       pb.GetProgress(),
		Percentage:     pb.GetPercentage(),
		SizeDownloaded: pb.GetSizeDownloaded(),
		ETA:            pb.GetEta(),
		Speed:          pb.GetSpeed(),
		CompletedOn:    time.Unix(pb.GetCompletedOnUnix(), 0),
		IsBad:          pb.GetIsBad(),
		Storage:        pb.GetStorage(),
		FailMessage:    pb.GetFailMessage(),
		Password:       pb.GetPassword(),
	}

	nzb.Files = make([]storage.NZBFile, len(pb.GetFiles()))
	for i, f := range pb.GetFiles() {
		nzb.Files[i] = protoToNZBFile(f)
	}

	return nzb
}

func protoToNZBFile(pb *NZBFileProto) storage.NZBFile {
	f := storage.NZBFile{
		NzbID:         pb.GetNzbId(),
		Name:          pb.GetName(),
		InternalPath:  pb.GetInternalPath(),
		Size:          pb.GetSize(),
		StartOffset:   pb.GetStartOffset(),
		Groups:        pb.GetGroups(),
		FileType:      storage.NZBFileType(pb.GetFileType()),
		Password:      pb.GetPassword(),
		IsDeleted:     pb.GetIsDeleted(),
		IsStored:      pb.GetIsStored(),
		SegmentSize:   pb.GetSegmentSize(),
		EncryptionKey: pb.GetEncryptionKey(),
		EncryptionIV:  pb.GetEncryptionIv(),
		IsEncrypted:   pb.GetIsEncrypted(),
	}

	f.Segments = make([]storage.NZBSegment, len(pb.GetSegments()))
	for i, s := range pb.GetSegments() {
		f.Segments[i] = storage.NZBSegment{
			Number:           int(s.GetNumber()),
			MessageID:        s.GetMessageId(),
			Bytes:            s.GetBytes(),
			StartOffset:      s.GetStartOffset(),
			EndOffset:        s.GetEndOffset(),
			Group:            s.GetGroup(),
			SegmentDataStart: s.GetSegmentDataStart(),
		}
	}

	return f
}
