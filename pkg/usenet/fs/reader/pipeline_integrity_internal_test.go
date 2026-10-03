package reader

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	nntpyenc "github.com/sirrobot01/decypharr/internal/nntp/yenc"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
)

func newPipelineIntegrityReader(
	t *testing.T,
	providers []config.UsenetProvider,
	segments []SegmentMeta,
) *StreamingReader {
	t.Helper()
	client, err := nntp.NewClient(&config.Config{Usenet: config.Usenet{Providers: providers}})
	if err != nil {
		t.Fatal(err)
	}
	sr, err := NewStreamingReader(t.Context(), client, segments,
		WithRetention(RetentionDelivery), WithMaxConnections(2), WithPrefetchAhead(0),
		WithBodyPipelineDepth(2), func(c *Config) { c.DownloadTimeout = 5 * time.Second })
	if err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if joinErr := errors.Join(sr.Close(), client.Close()); joinErr != nil {
			t.Error(joinErr)
		}
		if pool := sr.cache.extentPool.stats(); pool.MemoryInUse != 0 || pool.Caches != 0 ||
			sr.cache.residentN.Load() != 0 {
			t.Errorf("cache ownership after close: %+v, resident=%d", pool, sr.cache.residentN.Load())
		}
	})
	return sr
}

func pipelineIntegritySegments(size, dataStart int) []SegmentMeta {
	return []SegmentMeta{
		{
			MessageID:        "<zero@integrity>",
			Number:           1,
			Bytes:            int64(size),
			StartOffset:      0,
			EndOffset:        int64(size - 1),
			SegmentDataStart: int64(dataStart),
		},
		{
			MessageID:   "<one@integrity>",
			Number:      2,
			Bytes:       int64(size),
			StartOffset: int64(size),
			EndOffset:   int64(2*size - 1),
		},
	}
}

func corruptPipelineIntegrityBody(t *testing.T, payload []byte, part int, total, offset int64) []byte {
	t.Helper()
	corrupt := bytes.Clone(payload)
	for i := range corrupt {
		corrupt[i] ^= 0x5a
	}
	body := nntpd.Encode(corrupt, "retry.bin", part, total, offset)
	trailer := []byte(fmt.Sprintf("pcrc32=%08x", crc32.ChecksumIEEE(corrupt)))
	if bytes.Count(body, trailer) != 1 {
		t.Fatal("missing unique CRC trailer")
	}
	return bytes.Replace(body, trailer, fmt.Appendf(nil, "pcrc32=%08x", crc32.ChecksumIEEE(payload)), 1)
}

func TestPipelineRetryPreservesAcceptedBuffers(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"corrupt-redundant-copy", "adopt-rejected-primary", "pending-crc-error"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			checkPipelineRetry(t, mode)
		})
	}
}

// newTestNNTPServer starts a fake NNTP server closed with the test.
func newTestNNTPServer(t *testing.T) *nntpd.Server {
	t.Helper()
	srv, err := nntpd.New(nntpd.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv
}

// primaryAndBackup lists a primary provider and a backup provider.
func primaryAndBackup(primary *nntpd.Server, backupHost string, backupPort int, name string) []config.UsenetProvider {
	primaryHost, primaryPort := primary.Addr()
	return []config.UsenetProvider{
		{Host: primaryHost, Port: primaryPort, Backbone: name + "-primary", Priority: 1, MaxConnections: 1},
		{
			Host:           backupHost,
			Port:           backupPort,
			Backbone:       name + "-backup",
			Priority:       2,
			Backup:         true,
			MaxConnections: 1,
		},
	}
}

func checkPipelineRetry(t *testing.T, mode string) {
	t.Helper()
	const size = 64 << 10
	primary, backup := newTestNNTPServer(t), newTestNNTPServer(t)
	dataStart := 0
	if mode == "adopt-rejected-primary" {
		dataStart = size / 2
	}
	segments := pipelineIntegritySegments(size, dataStart)
	first := nntpd.Pattern(0, size+dataStart)
	second := nntpd.Pattern(int64(len(first)), size)
	total := int64(len(first) + len(second))
	primaryFirst := first
	if dataStart > 0 {
		primaryFirst = first[:dataStart/2]
	}
	primary.AddArticle(segments[0].MessageID, nntpd.Encode(primaryFirst, "retry.bin", 1, total, 0))
	backupFirst := nntpd.Encode(first, "retry.bin", 1, total, 0)
	if dataStart == 0 {
		backupFirst = corruptPipelineIntegrityBody(t, first, 1, total, 0)
	}
	backup.AddArticle(segments[0].MessageID, backupFirst)
	backupSecond := nntpd.Encode(second, "retry.bin", 2, total, int64(len(first)))
	if mode == "pending-crc-error" {
		backupSecond = corruptPipelineIntegrityBody(t, second, 2, total, int64(len(first)))
	}
	backup.AddArticle(segments[1].MessageID, backupSecond)
	backupHost, backupPort := backup.Addr()
	sr := newPipelineIntegrityReader(t, primaryAndBackup(primary, backupHost, backupPort, "integrity"), segments)

	ctx, cancel := context.WithTimeout(sr.ctx, 5*time.Second)
	defer cancel()
	fetchErr := sr.fetcher.fetchPrefetchBatch(ctx, []int{0, 1})
	pendingFailure := mode == "pending-crc-error"
	if pendingFailure {
		if !errors.Is(fetchErr, nntpyenc.ErrCrcMismatch) || !errors.Is(fetchErr, nntp.ErrAllProvidersFailed) ||
			!strings.Contains(fetchErr.Error(), "article 2/2:") {
			t.Fatalf("pending error lost identity or index: %v", fetchErr)
		}
	} else if fetchErr != nil {
		t.Fatalf("recovery: %v", fetchErr)
	}
	checkRetrySlot(t, sr, 0, first[dataStart:], false)
	checkRetrySlot(t, sr, 1, second, pendingFailure)

	if closeErr := sr.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	primary.Close()
	backup.Close()
	wantBackup := int64(1)
	if dataStart > 0 {
		wantBackup = 2
	}
	if primary.CompletedBodies.Load() != 1 || backup.CompletedBodies.Load() != wantBackup {
		t.Errorf(
			"completed BODYs: primary=%d, backup=%d, want 1/%d",
			primary.CompletedBodies.Load(),
			backup.CompletedBodies.Load(),
			wantBackup,
		)
	}
}

// checkRetrySlot requires slot i to hold exactly want, or to have failed
// without publishing anything.
func checkRetrySlot(t *testing.T, sr *StreamingReader, i int, want []byte, wantFailed bool) {
	t.Helper()
	dst := make([]byte, len(want))
	n, present := sr.cache.ReadRangeInto(i, 0, int64(len(want)), dst)
	if wantFailed {
		if present || n != 0 || sr.cache.GetState(i) != StateFailed {
			t.Errorf("failed slot published: state=%s, n=%d, present=%t", sr.cache.GetState(i), n, present)
		}
		return
	}
	if !present || n != len(want) || !bytes.Equal(dst, want) {
		t.Errorf(
			"slot %d: state=%s, n=%d, present=%t, exact=%t",
			i,
			sr.cache.GetState(i),
			n,
			present,
			bytes.Equal(dst, want),
		)
	}
}

func TestPipelineAcceptedBufferRemainsPrivateDuringRecovery(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"disconnect", "cancel", "reader-close", "idle"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			checkPipelineRecovery(t, action)
		})
	}
}

// gatedBackup is a backup provider that parks the first BODY until release.
type gatedBackup struct {
	port    int
	arrived chan string
	release func()
	done    chan error
}

// startGatedBackup serves one connection: it answers DATE, reports the first
// other command on arrived, waits for release, then acts per action.
func startGatedBackup(t *testing.T, action string, second []byte, secondID string) *gatedBackup {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	backup := &gatedBackup{
		port:    listener.Addr().(*net.TCPAddr).Port,
		arrived: make(chan string, 1),
		release: sync.OnceFunc(func() { close(gate) }),
		done:    make(chan error, 1),
	}
	var servers sync.WaitGroup
	servers.Go(func() {
		backup.done <- serveGatedBackup(listener, backup.arrived, gate, action, second, secondID)
	})
	t.Cleanup(func() { backup.release(); _ = listener.Close(); servers.Wait() })
	return backup
}

func serveGatedBackup(
	listener net.Listener,
	arrived chan<- string,
	gate <-chan struct{},
	action string,
	second []byte,
	secondID string,
) error {
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = io.WriteString(conn, "200 pipeline recovery test\r\n"); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	line, err := readSkippingDate(conn, reader)
	if err != nil {
		return err
	}
	arrived <- line
	<-gate
	switch action {
	case "disconnect":
		return nil
	case "idle":
		size := int64(len(second))
		_, err = fmt.Fprintf(
			conn,
			"222 0 %s body\r\n%s.\r\n",
			secondID,
			nntpd.Encode(second, "retry.bin", 2, 2*size, size),
		)
		return err
	}
	if _, err = reader.ReadByte(); err == nil {
		return errors.New("canceled client sent an unexpected byte")
	}
	return nil
}

// readSkippingDate answers DATE probes and returns the next command line.
func readSkippingDate(conn net.Conn, reader *bufio.Reader) (string, error) {
	for {
		line, err := reader.ReadString('\n')
		if err != nil || line != "DATE\r\n" {
			return line, err
		}
		if _, err = io.WriteString(conn, "111 20260905220000\r\n"); err != nil {
			return "", err
		}
	}
}

func checkPipelineRecovery(t *testing.T, action string) {
	t.Helper()
	const size = 64 << 10
	primary := newTestNNTPServer(t)
	segments := pipelineIntegritySegments(size, 0)
	first, second := nntpd.Pattern(0, size), nntpd.Pattern(size, size)
	primary.AddArticle(segments[0].MessageID, nntpd.Encode(first, "retry.bin", 1, 2*size, 0))
	backup := startGatedBackup(t, action, second, segments[1].MessageID)
	defer backup.release()
	sr := newPipelineIntegrityReader(t, primaryAndBackup(primary, "127.0.0.1", backup.port, "staging"), segments)

	ctx, cancel := context.WithTimeout(sr.ctx, 5*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	if !sr.fetcher.submit(
		ctx,
		priorityPrefetch,
		func() { finished <- sr.fetcher.fetchPrefetchBatch(ctx, []int{0, 1}) },
		nil,
	) {
		t.Fatal("submission failed")
	}
	select {
	case line := <-backup.arrived:
		if line != "BODY "+segments[1].MessageID+"\r\n" {
			t.Fatalf("backup command = %q", line)
		}
	case finishedErr := <-finished:
		t.Fatalf("fetch ended before recovery gate: %v", finishedErr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	assertStagingPrivate(t, sr, size)

	switch action {
	case "cancel":
		cancel()
	case "reader-close":
		if closeErr := sr.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	case "idle":
		sr.cache.ReleaseIdleDelivery()
	}
	backup.release()
	var fetchErr error
	select {
	case fetchErr = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("fetch did not join")
	}
	checkRecoveryError(t, sr, action, fetchErr)
	if action == "cancel" || action == "disconnect" {
		dst := make([]byte, size)
		if n, present := sr.cache.ReadRangeInto(0, 0, size, dst); !present || n != size || !bytes.Equal(dst, first) {
			t.Fatalf("accepted peer lost after cancellation: n=%d, present=%t", n, present)
		}
	}
	if serverDoneErr := <-backup.done; serverDoneErr != nil {
		t.Fatal(serverDoneErr)
	}
}

// assertStagingPrivate requires accepted bytes to stay unpublished until the
// batch ends.
func assertStagingPrivate(t *testing.T, sr *StreamingReader, size int64) {
	t.Helper()
	dst := make([]byte, size)
	n, present := sr.cache.ReadRangeInto(0, 0, size, dst)
	if n != 0 || present || sr.cache.GetState(0) != StateFetching || sr.cache.residentN.Load() != 0 ||
		sr.cache.extentPool.inUse.Load() != 0 {
		t.Fatalf(
			"accepted bytes published before batch end: n=%d, present=%t, state=%s",
			n,
			present,
			sr.cache.GetState(0),
		)
	}
}

// checkRecoveryError checks the batch's error identity for each action.
func checkRecoveryError(t *testing.T, sr *StreamingReader, action string, fetchErr error) {
	t.Helper()
	switch action {
	case "idle":
		if fetchErr != nil || sr.cache.residentN.Load() != 0 || sr.cache.extentPool.inUse.Load() != 0 {
			t.Fatalf("idle staging retained ownership: error=%v, resident=%d", fetchErr, sr.cache.residentN.Load())
		}
	case "disconnect":
		typed, ok := errors.AsType[*nntp.Error](fetchErr)
		if !ok || typed.Type != nntp.ErrorTypeConnection || !errors.Is(fetchErr, nntp.ErrAllProvidersFailed) ||
			!strings.Contains(fetchErr.Error(), "article 2/2:") {
			t.Fatalf("pending connection failure lost identity or index: %v", fetchErr)
		}
	default:
		if !errors.Is(fetchErr, context.Canceled) {
			t.Fatalf("cancellation identity = %v", fetchErr)
		}
	}
}
