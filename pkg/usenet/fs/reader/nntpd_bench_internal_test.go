package reader

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
)

const (
	benchSegSize = int64(750 * 1024)
	benchSegs    = 32
)

type benchMode struct {
	name   string
	memory bool
}

// benchModes compares immutable RAM extents with the sparse rewind tier.
func benchModes() []benchMode {
	return []benchMode{{"disk", false}, {"memory", true}}
}

// benchStack is what every reader of one sub-benchmark shares, as the readers
// of a running service share one client, one set of pools and one scheduler.
type benchStack struct {
	client    *nntp.Client
	segs      []SegmentMeta
	pools     *Pools
	scheduler *FetchScheduler
}

// newBenchStack builds a fake NNTP server carrying benchSegs segments of
// Pattern data, a client pointed at it, and the shared pools and scheduler.
func newBenchStack(b *testing.B, cfg nntpd.Config) benchStack {
	b.Helper()
	client, segs := newBenchServer(b, cfg)
	pools, _ := newTestPools(b, 256<<20)
	scheduler, _, _ := newTestScheduler(b, newTestRoot(b), 8)
	return benchStack{client: client, segs: segs, pools: pools, scheduler: scheduler}
}

// stopBenchReader stops sr and waits for its shutdown, so the next timed
// iteration does not overlap the previous reader's teardown.
func stopBenchReader(b *testing.B, sr *StreamingReader) {
	b.Helper()
	if err := sr.Stop(b.Context()); err != nil {
		b.Fatal(err)
	}
}

func newBenchServer(b *testing.B, cfg nntpd.Config) (*nntp.Client, []SegmentMeta) {
	b.Helper()
	srv, err := nntpd.New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(srv.Close)

	segs := make([]SegmentMeta, benchSegs)
	fileSize := benchSegSize * benchSegs
	for i := range segs {
		off := int64(i) * benchSegSize
		id := fmt.Sprintf("<seg%d@nntpd>", i)
		srv.AddArticle(id, nntpd.Encode(nntpd.Pattern(off, int(benchSegSize)), "bench.bin", i+1, fileSize, off))
		segs[i] = SegmentMeta{
			MessageID:   id,
			Number:      i + 1,
			Bytes:       benchSegSize,
			StartOffset: off,
			EndOffset:   off + benchSegSize - 1,
		}
	}

	host, port := srv.Addr()
	client, err := nntp.NewClient(newClientGroup(b), &config.Config{
		Usenet: config.Usenet{
			Providers: []config.UsenetProvider{{
				Host:           host,
				Port:           port,
				MaxConnections: 8,
			}},
		},
	}, zerolog.Nop())
	if err != nil {
		b.Fatal(err)
	}
	return client, segs
}

func newBenchReader(b *testing.B, stack benchStack, memory bool, diskPath string) *StreamingReader {
	b.Helper()
	retention := RetentionRewind
	if memory {
		retention = RetentionWindow
	}
	sr, err := NewStreamingReader(context.Background(), stack.client, stack.segs,
		WithPools(stack.pools), WithFetchScheduler(stack.scheduler),
		WithDiskPath(diskPath),
		WithMaxConnections(8),
		WithPrefetchAhead(8),
		WithRetention(retention),
	)
	if err != nil {
		b.Fatal(err)
	}
	return sr
}

// diskAllocatedMB reports the allocated (non-hole) bytes of the cache's
// segments.bin under dir. st.Blocks counts 512-byte units actually backed by
// the filesystem, so a sparse file that was never written reports ~0 no
// matter its logical size — this is the metric that proves memory mode kept
// segment data off the disk.
func diskAllocatedMB(dir string) float64 {
	matches, _ := filepath.Glob(filepath.Join(dir, "cache-*", "segments.bin"))
	var total int64
	for _, m := range matches {
		var st syscall.Stat_t
		if err := syscall.Stat(m, &st); err == nil {
			total += st.Blocks * 512
		}
	}
	return float64(total) / (1 << 20)
}

// poolRAMMB reports resident bytes owned by both Usenet storage tiers.
func poolRAMMB(sr *StreamingReader) float64 {
	bufferBytes := sr.cache.pools.buffers.Stats().MemoryInUse
	extentBytes := sr.cache.pools.extents.stats().MemoryInUse
	return float64(bufferBytes+extentBytes) / (1 << 20)
}

// BenchmarkColdStream reads the whole file sequentially through a fresh
// reader+cache each iteration: fetch, decode, cache write, cache read.
func BenchmarkColdStream(b *testing.B) {
	for _, mode := range benchModes() {
		b.Run(mode.name, func(b *testing.B) {
			stack := newBenchStack(b, nntpd.Config{})
			fileSize := benchSegSize * benchSegs
			buf := make([]byte, 128*1024)

			var peakPoolMB, peakDiskMB float64
			b.ReportAllocs()
			b.SetBytes(fileSize)
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				dir := b.TempDir()
				sr := newBenchReader(b, stack, mode.memory, dir)
				b.StartTimer()

				for off := int64(0); off < fileSize; off += int64(len(buf)) {
					n, err := sr.ReadAt(buf[:min(int64(len(buf)), fileSize-off)], off)
					if err != nil {
						b.Fatal(err)
					}
					if off == 0 && !bytes.Equal(buf[:n], nntpd.Pattern(0, n)) {
						b.Fatal("payload mismatch at offset 0")
					}
				}

				b.StopTimer()
				peakPoolMB = max(peakPoolMB, poolRAMMB(sr))
				peakDiskMB = max(peakDiskMB, diskAllocatedMB(dir))
				stopBenchReader(b, sr)
				b.StartTimer()
			}
			b.ReportMetric(peakPoolMB, "pool-ram-MB")
			b.ReportMetric(peakDiskMB, "disk-MB")
		})
	}
}

// BenchmarkConfiguredBodyPipelineDepth exercises the user-facing reader
// option through a cold sequential stream. A full-file read gives the initial
// read-ahead window enough work to use every configured pipeline depth.
func BenchmarkConfiguredBodyPipelineDepth(b *testing.B) {
	const prefetchAhead = benchSegs - 1
	for _, depth := range []int{1, 2, 4} {
		b.Run(fmt.Sprintf("depth%d", depth), func(b *testing.B) {
			stack := newBenchStack(b, nntpd.Config{RTT: 30 * time.Millisecond})
			fileSize := benchSegSize * benchSegs
			buf := make([]byte, 128*1024)

			b.ReportAllocs()
			b.SetBytes(fileSize)
			for b.Loop() {
				b.StopTimer()
				sr, err := NewStreamingReader(b.Context(), stack.client, stack.segs,
					WithPools(stack.pools), WithFetchScheduler(stack.scheduler),
					WithDiskPath(b.TempDir()),
					WithMaxConnections(8),
					WithPrefetchAhead(prefetchAhead),
					WithBodyPipelineDepth(depth),
					WithRetention(RetentionWindow),
				)
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()

				readSequential(b, sr, buf, fileSize)

				b.StopTimer()
				stopBenchReader(b, sr)
				b.StartTimer()
			}
		})
	}
}

// BenchmarkOpenToFirstByte measures reader construction plus the first 64KB
// read — the time-to-first-byte a mount open pays.
func BenchmarkOpenToFirstByte(b *testing.B) {
	for _, mode := range benchModes() {
		b.Run(mode.name, func(b *testing.B) {
			stack := newBenchStack(b, nntpd.Config{})
			buf := make([]byte, 64*1024)

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				dir := b.TempDir()
				b.StartTimer()
				sr := newBenchReader(b, stack, mode.memory, dir)
				if _, err := sr.ReadAt(buf, 0); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				stopBenchReader(b, sr)
				b.StartTimer()
			}
		})
	}
}

// BenchmarkSeekLatency measures the first-byte latency of 64KB reads landing
// on uncached segments while prefetch churn runs, as during real playback
// seeks. Segments are visited in descending order: the prefetch window only
// reaches forward, so every target stays cold, while the prefetcher still
// burns slots on segments the next seek abandons. Reports p50/p99 alongside
// the mean.
func BenchmarkSeekLatency(b *testing.B) {
	for _, rtt := range []time.Duration{0, 30 * time.Millisecond} {
		b.Run(fmt.Sprintf("rtt%dms", rtt/time.Millisecond), func(b *testing.B) {
			for _, mode := range benchModes() {
				b.Run(mode.name, func(b *testing.B) {
					benchmarkSeekLatency(b, rtt, mode.memory)
				})
			}
		})
	}
}

func benchmarkSeekLatency(b *testing.B, rtt time.Duration, memory bool) {
	stack := newBenchStack(b, nntpd.Config{RTT: rtt})
	buf := make([]byte, 64*1024)

	var durations []time.Duration
	var sr *StreamingReader
	visited := benchSegs // force a fresh reader on first iteration

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if visited == benchSegs {
			b.StopTimer()
			if sr != nil {
				stopBenchReader(b, sr)
			}
			sr = newBenchReader(b, stack, memory, b.TempDir())
			visited = 0
			b.StartTimer()
		}
		seg := benchSegs - 1 - (i % benchSegs)
		start := time.Now()
		if _, err := sr.ReadAt(buf, int64(seg)*benchSegSize); err != nil {
			b.Fatal(err)
		}
		durations = append(durations, time.Since(start))
		visited++
	}
	b.StopTimer()
	if sr != nil {
		stopBenchReader(b, sr)
	}
	reportPercentiles(b, durations)
}

// reportPercentiles reports p50/p99 of durations in milliseconds.
func reportPercentiles(b *testing.B, durations []time.Duration) {
	if len(durations) == 0 {
		return
	}
	slices.Sort(durations)
	p50 := durations[len(durations)/2]
	p99 := durations[len(durations)*99/100]
	b.ReportMetric(float64(p50.Microseconds())/1000, "p50-ms")
	b.ReportMetric(float64(p99.Microseconds())/1000, "p99-ms")
}

// readSequential reads the whole file through buf.
func readSequential(b *testing.B, sr *StreamingReader, buf []byte, fileSize int64) {
	for off := int64(0); off < fileSize; off += int64(len(buf)) {
		if _, err := sr.ReadAt(buf[:min(int64(len(buf)), fileSize-off)], off); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWarmReread reads a fully cached file, isolating the cache read
// path from any network or decode work.
func BenchmarkWarmReread(b *testing.B) {
	for _, mode := range benchModes() {
		b.Run(mode.name, func(b *testing.B) {
			stack := newBenchStack(b, nntpd.Config{})
			fileSize := benchSegSize * benchSegs
			buf := make([]byte, 128*1024)

			dir := b.TempDir()
			sr := newBenchReader(b, stack, mode.memory, dir)
			defer stopBenchReader(b, sr)
			readAll := func() {
				for off := int64(0); off < fileSize; off += int64(len(buf)) {
					if _, err := sr.ReadAt(buf[:min(int64(len(buf)), fileSize-off)], off); err != nil {
						b.Fatal(err)
					}
				}
			}
			readAll()

			b.ReportAllocs()
			b.SetBytes(fileSize)
			b.ResetTimer()
			for range b.N {
				readAll()
			}
			b.StopTimer()
			// After ResetTimer: it clears previously reported metrics.
			b.ReportMetric(poolRAMMB(sr), "pool-ram-MB")
			b.ReportMetric(diskAllocatedMB(dir), "disk-MB")
		})
	}
}
