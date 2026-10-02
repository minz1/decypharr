// Package nntpd is an in-process fake NNTP server for benchmarks and tests.
// It speaks just enough of the protocol for the client in internal/nntp —
// greeting, AUTHINFO USER/PASS, DATE, STAT, BODY, QUIT — serving pre-encoded
// yEnc articles with configurable per-response RTT and per-connection
// bandwidth.
package nntpd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config controls simulated network behavior.
type Config struct {
	// RTT is the artificial delay applied before the first response to each
	// command burst. Commands delivered in one pipeline share the delay.
	RTT time.Duration
	// Bandwidth caps body streaming per connection in bytes/second.
	// 0 means unlimited.
	Bandwidth int64
}

// Server listens on a loopback port until Close.
type Server struct {
	cfg      Config
	ln       net.Listener
	mu       sync.Mutex
	articles map[string][]byte
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	closed   atomic.Bool
	// Bodies counts existing-article BODY attempts before writing the response.
	Bodies atomic.Int64
	// CompletedBodies and CompletedBodyBytes advance after the response flush
	// succeeds. Body bytes include yEnc framing, excluding the status/terminator.
	CompletedBodies    atomic.Int64
	CompletedBodyBytes atomic.Int64
	// SocketBytes counts bytes accepted by net.Conn.Write, including protocol
	// framing and partial writes. It does not imply the client consumed them.
	SocketBytes atomic.Int64
}

const (
	readBufSize   = 4 << 10
	writeBufSize  = 256 << 10
	throttleChunk = 64 << 10

	// yEnc encoding parameters.
	yencOffset     = 42  // added to every byte
	yencEscapeAdd  = 64  // added to an escaped byte
	yencLineLength = 128 // encoded columns per line
	// patternModulus is prime so Pattern does not repeat at power-of-two
	// boundaries.
	patternModulus = 251
)

type socketWriter struct {
	conn  net.Conn
	bytes *atomic.Int64
}

func (w socketWriter) Write(p []byte) (int, error) {
	n, err := w.conn.Write(p)
	w.bytes.Add(int64(n))
	return n, err
}

// New starts a server on a random loopback port.
func New(cfg Config) (*Server, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:      cfg,
		ln:       ln,
		articles: make(map[string][]byte),
		conns:    make(map[net.Conn]struct{}),
	}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Addr returns the host and port the server listens on.
func (s *Server) Addr() (string, int) {
	addr, ok := s.ln.Addr().(*net.TCPAddr)
	if !ok {
		panic("nntpd: listener is not TCP")
	}
	return "127.0.0.1", addr.Port
}

// AddArticle registers a pre-encoded yEnc body (without the ".\r\n"
// terminator) under messageID, which must include the angle brackets.
func (s *Server) AddArticle(messageID string, encodedBody []byte) {
	s.mu.Lock()
	s.articles[messageID] = encodedBody
	s.mu.Unlock()
}

// Close stops the listener and tears down every open connection.
func (s *Server) Close() {
	if s.closed.Swap(true) {
		return
	}
	_ = s.ln.Close()
	s.mu.Lock()
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		// Close may have swept conns between Accept and here; a conn
		// registered after the sweep would never close and Close would hang.
		if s.closed.Load() {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.Close()
		s.wg.Done()
	}()

	reader := bufio.NewReaderSize(conn, readBufSize)
	writer := bufio.NewWriterSize(socketWriter{conn, &s.SocketBytes}, writeBufSize)

	if s.respond(writer, "200 nntpd ready") != nil {
		return
	}
	inPipeline := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if !inPipeline {
			s.sleepRTT()
		}
		inPipeline = reader.Buffered() > 0

		if quit, cmdErr := s.handleCommand(writer, fields); quit || cmdErr != nil {
			return
		}
	}
}

// handleCommand answers one command line. quit reports that the connection
// should close.
func (s *Server) handleCommand(writer *bufio.Writer, fields []string) (bool, error) {
	var arg string
	if len(fields) > 1 {
		arg = fields[len(fields)-1]
	}
	switch strings.ToUpper(fields[0]) {
	case "AUTHINFO":
		if len(fields) > 1 && strings.EqualFold(fields[1], "USER") {
			return false, writeResponse(writer, "381 password required")
		}
		return false, writeResponse(writer, "281 authentication accepted")
	case "DATE":
		return false, writeResponse(writer, "111 20260101000000")
	case "STAT":
		if s.lookup(arg) != nil {
			return false, writeResponse(writer, "223 0 "+arg)
		}
		return false, writeResponse(writer, "430 no such article")
	case "BODY":
		return false, s.writeBody(writer, arg)
	case "QUIT":
		_ = writeResponse(writer, "205 bye")
		return true, nil
	default:
		return false, writeResponse(writer, "500 unknown command")
	}
}

// writeBody sends the article body for messageID, or 430 when unknown.
func (s *Server) writeBody(writer *bufio.Writer, messageID string) error {
	body := s.lookup(messageID)
	if body == nil {
		return writeResponse(writer, "430 no such article")
	}
	s.Bodies.Add(1)
	if _, err := writer.WriteString("222 0 " + messageID + " body\r\n"); err != nil {
		return err
	}
	if err := s.writeThrottled(writer, body); err != nil {
		return err
	}
	if _, err := writer.WriteString(".\r\n"); err != nil {
		return err
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	s.CompletedBodies.Add(1)
	s.CompletedBodyBytes.Add(int64(len(body)))
	return nil
}

func (s *Server) lookup(messageID string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.articles[messageID]
}

func (s *Server) respond(w *bufio.Writer, line string) error {
	s.sleepRTT()
	return writeResponse(w, line)
}

func writeResponse(w *bufio.Writer, line string) error {
	if _, err := w.WriteString(line + "\r\n"); err != nil {
		return err
	}
	return w.Flush()
}

func (s *Server) sleepRTT() {
	if s.cfg.RTT > 0 {
		time.Sleep(s.cfg.RTT)
	}
}

// writeThrottled streams data, pacing flushes to the configured bandwidth.
func (s *Server) writeThrottled(w *bufio.Writer, data []byte) error {
	if s.cfg.Bandwidth <= 0 {
		_, err := w.Write(data)
		return err
	}
	const chunk = throttleChunk
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		if _, err := w.Write(data[off:end]); err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return err
		}
		time.Sleep(time.Duration(float64(end-off) / float64(s.cfg.Bandwidth) * float64(time.Second)))
	}
	return nil
}

// Pattern returns deterministic bytes addressable by file offset, so reads
// can be verified at any position.
func Pattern(offset int64, n int) []byte {
	p := make([]byte, n)
	for i := range p {
		v := (offset + int64(i)) % patternModulus
		if v < 0 || v > math.MaxUint8 {
			panic("nntpd: Pattern needs a non-negative offset")
		}
		p[i] = byte(v)
	}
	return p
}

// Encode produces the yEnc-encoded article body for one part of a file,
// with a correct pcrc32 so decodes exercise CRC verification. offset is the
// part's start within the file, fileSize the whole file's size.
func Encode(payload []byte, name string, part int, fileSize, offset int64) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "=ybegin part=%d line=128 size=%d name=%s\r\n", part, fileSize, name)
	fmt.Fprintf(&buf, "=ypart begin=%d end=%d\r\n", offset+1, offset+int64(len(payload)))
	col := 0
	for _, b := range payload {
		e := b + yencOffset
		if e == 0 || e == '\n' || e == '\r' || e == '=' || e == '\t' || e == ' ' || e == '.' {
			buf.WriteByte('=')
			buf.WriteByte(e + yencEscapeAdd)
			col += 2
		} else {
			buf.WriteByte(e)
			col++
		}
		if col >= yencLineLength {
			buf.WriteString("\r\n")
			col = 0
		}
	}
	if col > 0 {
		buf.WriteString("\r\n")
	}
	fmt.Fprintf(&buf, "=yend size=%d part=%d pcrc32=%08x\r\n", len(payload), part, crc32.ChecksumIEEE(payload))
	return buf.Bytes()
}
