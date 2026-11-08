package usenet

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/fs/reader"
)

// probeCancelBound is how long a probe may take to return once its context is cancelled. The probe waits on a
// server that never answers, so only the cancellation can end it.
const probeCancelBound = 10 * time.Second

// silentServer is an NNTP server that greets and answers session commands but never answers STAT or BODY, as a
// provider that stops responding mid-probe would. Each unanswered command is reported on asked.
type silentServer struct {
	ln    net.Listener
	asked chan string
	wg    sync.WaitGroup
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func newSilentServer(t *testing.T) *silentServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &silentServer{ln: ln, asked: make(chan string, 64), conns: map[net.Conn]struct{}{}}
	s.wg.Go(s.accept)
	t.Cleanup(s.close)
	return s
}

func (s *silentServer) addr() (string, int) {
	addr, ok := s.ln.Addr().(*net.TCPAddr)
	if !ok {
		panic("silentServer: listener is not TCP")
	}
	return "127.0.0.1", addr.Port
}

func (s *silentServer) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Go(func() { s.serve(conn) })
	}
}

func (s *silentServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprint(conn, "200 silent ready\r\n"); err != nil {
		return
	}
	lines := bufio.NewReader(conn)
	for {
		line, err := lines.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		var reply string
		switch strings.ToUpper(fields[0]) {
		case "STAT", "BODY", "ARTICLE", "HEAD":
			select {
			case s.asked <- strings.ToUpper(fields[0]):
			default:
			}
			continue // never answered
		case "AUTHINFO":
			reply = "281 authentication accepted"
		case "DATE":
			reply = "111 20260101000000"
		case "QUIT":
			_, _ = fmt.Fprint(conn, "205 bye\r\n")
			return
		default:
			reply = "500 unknown command"
		}
		if _, writeErr := fmt.Fprint(conn, reply+"\r\n"); writeErr != nil {
			return
		}
	}
}

func (s *silentServer) close() {
	_ = s.ln.Close()
	s.mu.Lock()
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// newSilentUsenet builds a usenet service on a silent server with one stored media file of two segments.
func newSilentUsenet(t *testing.T, srv *silentServer) *Usenet {
	t.Helper()
	const segSize = 32 << 10
	file := storage.NZBFile{Name: "movie.mkv", Size: 2 * segSize, FileType: storage.NZBFileTypeMedia, IsStored: true}
	for i := range 2 {
		off := int64(i * segSize)
		file.Segments = append(file.Segments, storage.NZBSegment{
			Number: i + 1, MessageID: fmt.Sprintf("<silent-%d@test>", i), Bytes: segSize,
			StartOffset: off, EndOffset: off + segSize - 1,
		})
	}

	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() {
		ctx, cancel := task.Detached(t.Context(), 30*time.Second)
		defer cancel()
		_ = g.Stop(ctx)
	})
	host, port := srv.addr()
	client, err := nntp.NewClient(g.Child("nntp"), &config.Config{Usenet: config.Usenet{
		Providers: []config.UsenetProvider{{Host: host, Port: port, MaxConnections: 2}},
	}}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	metaStore := &NZBStorage{metaDir: t.TempDir(), codec: testCodec(t), logger: zerolog.Nop()}
	if addErr := metaStore.AddNZB(&storage.NZB{ID: "nzb", Files: []storage.NZBFile{file}}); addErr != nil {
		t.Fatal(addErr)
	}
	bufferPools, err := reader.NewPools(g, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := reader.NewFetchScheduler(g.Child("scheduler"), 2)
	if err != nil {
		t.Fatal(err)
	}
	return &Usenet{
		work:                     g.Child("work"),
		bufferPools:              bufferPools,
		fetchScheduler:           scheduler,
		nntp:                     client,
		nzbStorage:               metaStore,
		config:                   config.NewStore(&config.Config{}),
		logger:                   zerolog.Nop(),
		maxConnections:           2,
		processingMaxConnections: 2,
		prefetchSize:             1 << 20,
	}
}

// The repair probes of a usenet file (CheckFile's article STATs, VerifyFile's head read) end when their context is
// cancelled, even while the provider never answers: a stop or a restart never waits on a silent provider.
func TestUsenetProbesReturnOnCancel(t *testing.T) {
	t.Parallel()
	probes := map[string]func(*Usenet, context.Context) error{
		"CheckFile":  func(u *Usenet, ctx context.Context) error { return u.CheckFile(ctx, "nzb", "movie.mkv") },
		"VerifyFile": func(u *Usenet, ctx context.Context) error { return u.VerifyFile(ctx, "nzb", "movie.mkv") },
	}
	for name, probe := range probes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newSilentServer(t)
			u := newSilentUsenet(t, srv)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- probe(u, ctx) }()

			select {
			case <-srv.asked: // the probe is waiting on the provider
			case err := <-done:
				t.Fatalf("%s returned %v before asking the provider", name, err)
			case <-time.After(probeCancelBound):
				t.Fatalf("%s never asked the provider", name)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(probeCancelBound):
				t.Fatalf("%s did not return within %s of its cancellation", name, probeCancelBound)
			}
		})
	}
}
