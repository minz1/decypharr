package fs

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"go4.org/readerutil"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/pkg/usenet/fs/reader"
	"github.com/sirrobot01/decypharr/pkg/usenet/types"
)

// PrefetchableReaderAt extends [io.ReaderAt] with prefetch capability.
// This allows callers to trigger segment downloads before starting reads.
type PrefetchableReaderAt interface {
	io.ReaderAt
	// ReadAtContext reads with caller cancellation.
	ReadAtContext(ctx context.Context, p []byte, off int64) (int, error)
	// Prefetch triggers segment downloads for the given byte range without blocking.
	Prefetch(ct context.Context, off, length int64)
	// OpenCursor returns an independent read cursor over the same data.
	// Each concurrent consumer should use its own cursor so seek detection
	// and eviction-window tracking don't interfere across consumers.
	OpenCursor() reader.ReadCursor
}

// readerSettings are the knobs every volume's streaming reader shares.
type readerSettings struct {
	pools             *reader.Pools
	ctx               context.Context
	client            *nntp.Client // Connection client for all readers
	maxConcurrent     int          // Scheduler width for standalone readers
	prefetchSize      int64        // Prefetch size in bytes
	bodyPipelineDepth int
	diskPath          string
	retention         reader.Retention
	scheduler         *reader.FetchScheduler
	logger            zerolog.Logger
}

// newReader builds the (optionally decrypting) streaming reader for vol.
func (s *readerSettings) newReader(vol *types.Volume) (*reader.StreamingReader, error) {
	if s.client == nil {
		return nil, fmt.Errorf("no connection client available for streaming reader")
	}
	segments := reader.VolumeToSegmentMeta(vol)
	if len(segments) == 0 {
		return nil, fmt.Errorf("no segments in volume %s", vol.Name)
	}
	options := []reader.Option{
		reader.WithMaxConnections(s.maxConcurrent),
		reader.WithPrefetchAhead(reader.PrefetchAheadSegments(s.prefetchSize, segments)),
		reader.WithBodyPipelineDepth(s.bodyPipelineDepth),
		reader.WithDiskPath(s.diskPath),
		reader.WithRetention(s.retention),
		reader.WithFetchScheduler(s.scheduler),
		reader.WithPools(s.pools),
	}
	if encryption := reader.EncryptionFromVolume(vol); encryption.Enabled {
		return reader.NewStreamingReaderWithEncryption(s.ctx, s.client, segments, encryption, options...)
	}
	return reader.NewStreamingReader(s.ctx, s.client, segments, options...)
}

// FS implements [fs.FS] for RAR volumes backed by NNTP Segments.
type FS struct {
	readerSettings

	volumes *xsync.Map[string, *types.Volume]
}

// Option configures the filesystem.
type Option func(*FS)

func WithRetention(retention reader.Retention) Option {
	return func(f *FS) { f.retention = retention }
}

func WithFetchScheduler(scheduler *reader.FetchScheduler) Option {
	return func(f *FS) { f.scheduler = scheduler }
}

// WithPools shares the service cache budgets with this filesystem.
func WithPools(pools *reader.Pools) Option { return func(f *FS) { f.pools = pools } }

// WithBodyPipelineDepth sets how many BODY commands each connection keeps in
// flight (usenet.body_pipeline_depth).
func WithBodyPipelineDepth(depth int) Option {
	return func(f *FS) { f.bodyPipelineDepth = depth }
}

// WithDiskPath spills stream buffers to files under path (usenet.disk_path);
// empty keeps them in memory.
func WithDiskPath(path string) Option { return func(f *FS) { f.diskPath = path } }

// NewFS creates a new filesystem backed by the provided connection nntpClient.
// prefetchSize is the amount of data to prefetch ahead in bytes (e.g., 16*1024*1024 for 16MB).
func NewFS(
	ctx context.Context,
	client *nntp.Client,
	maxConcurrent int,
	prefetchSize int64,
	volumes []*types.Volume,
	logger zerolog.Logger,
	opts ...Option,
) (*FS, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	f := &FS{
		ctx:               ctx,
		client:            client,
		maxConcurrent:     maxConcurrent,
		prefetchSize:      prefetchSize,
		bodyPipelineDepth: config.DefaultBodyPipelineDepth,
		retention:         reader.RetentionWindow,
		logger:            logger,
		volumes:           xsync.NewMap[string, *types.Volume](),
	}

	// Apply options
	for _, opt := range opts {
		opt(f)
	}

	for _, vol := range volumes {
		f.registerVolume(vol)
	}

	return f, nil
}

// registerVolume adds or updates a Volume in the filesystem.
func (f *FS) registerVolume(vol *types.Volume) {
	key := normalizePath(vol.Name)

	f.volumes.Store(key, vol)
}

// Open implements [fs.FS] to provide access to virtual RAR volumes.
func (f *FS) Open(name string) (fs.File, error) {
	if name == "" {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	key := normalizePath(name)

	vol, found := f.volumes.Load(key)

	if !found {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	info := volumeInfo{
		name: name,
		size: vol.Size,
	}

	return &File{
		readerSettings: f.readerSettings,
		info:           info,
		volume:         vol,
	}, nil
}

func (f *FS) CreateReader() (io.Reader, int64, func(), error) {
	// Create a ReaderAt first
	readerAt, size, cleanup, err := f.CreateReaderAt()
	if err != nil {
		return nil, 0, nil, err
	}

	// Wrap ReaderAt in a Reader
	reader := io.NewSectionReader(readerAt, 0, size)

	return reader, size, cleanup, nil
}

func (f *FS) CreateReaderAt() (io.ReaderAt, int64, func(), error) {
	volumeSize := f.volumes.Size()
	if volumeSize == 0 {
		return nil, 0, nil, fmt.Errorf("no archive volumes available")
	}

	readers := make([]readerutil.SizeReaderAt, 0, volumeSize)
	closers := make([]io.Closer, 0, volumeSize)

	volumes := make([]*types.Volume, 0, volumeSize)
	f.volumes.Range(func(_ string, value *types.Volume) bool {
		volumes = append(volumes, value)
		return true
	})

	// Sort volumes by Index to ensure correct order
	sort.Slice(volumes, func(i, j int) bool {
		return volumes[i].Index < volumes[j].Index
	})

	for _, vol := range volumes {
		f, err := f.Open(vol.Name)
		if err != nil {
			closeArchiveClosers(closers)
			return nil, 0, nil, fmt.Errorf("open archive volume %s: %w", vol.Name, err)
		}
		closers = append(closers, f)

		readerAt, ok := f.(io.ReaderAt)
		if !ok {
			closeArchiveClosers(closers)
			return nil, 0, nil, fmt.Errorf("archive volume %s is not random-access capable", vol.Name)
		}

		readers = append(readers, io.NewSectionReader(readerAt, 0, vol.Size))
	}

	var (
		reader io.ReaderAt
		size   int64
	)

	if len(readers) == 1 {
		reader = readers[0]
		size = volumes[0].Size
	} else {
		mr := readerutil.NewMultiReaderAt(readers...)
		reader = mr
		size = mr.Size()
	}

	cleanup := func() {
		closeArchiveClosers(closers)
	}

	return reader, size, cleanup, nil
}

// CreateReaderAtForVolume creates a reader for a single volume directly.
// This is an optimization for the common streaming case where there's only one volume.
// Returns PrefetchableReaderAt so callers can trigger prefetch before starting reads.
func (f *FS) CreateReaderAtForVolume(vol *types.Volume) (PrefetchableReaderAt, int64, func(), error) {
	if vol == nil || len(vol.Segments) == 0 {
		return nil, 0, nil, fmt.Errorf("no segments in volume")
	}

	return f.createNewReaderForVolume(vol)
}

// createNewReaderForVolume wraps the volume's streaming reader with its cleanup.
func (f *FS) createNewReaderForVolume(vol *types.Volume) (PrefetchableReaderAt, int64, func(), error) {
	streamReader, err := f.newReader(vol)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("failed to create new streaming reader for volume %s: %w", vol.Name, err)
	}
	cleanup := func() {
		_ = streamReader.Close()
	}
	return streamReader, vol.Size, cleanup, nil
}

func closeArchiveClosers(closers []io.Closer) {
	for _, closer := range closers {
		if closer != nil {
			_ = closer.Close()
		}
	}
}

func normalizePath(path string) string {
	if path == "" {
		return path
	}
	return filepath.Clean(path)
}
