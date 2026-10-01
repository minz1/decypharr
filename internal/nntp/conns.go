package nntp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	nntpyenc "github.com/sirrobot01/decypharr/internal/nntp/yenc"
)

// bodyBufInitialCap covers a typical ~750KB usenet segment.
const bodyBufInitialCap = 1 << 20

// bodyBufPool reuses storage for decoded articles not retained by a caller.
//
//nolint:gochecknoglobals // a sync.Pool only pays off when shared by every connection
var bodyBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, bodyBufInitialCap)
		return &b
	},
}

func getBodyBuf() []byte {
	if b, ok := bodyBufPool.Get().(*[]byte); ok {
		return *b
	}
	return make([]byte, 0, bodyBufInitialCap)
}

func putBodyBuf(b []byte) {
	if cap(b) == 0 {
		return
	}
	b = b[:0]
	bodyBufPool.Put(&b)
}

// DecodedBodyCapacity matches the decoder's initial growth policy. Supplying
// this capacity lets DecodeBodyInto keep the caller's allocation.
func DecodedBodyCapacity(decodedSize int64) int {
	const (
		chunk    = int64(32 * 1024)
		maxPart  = int64(10 * 1024 * 1024)
		minTotal = int64(1024)
	)
	if decodedSize < 0 {
		decodedSize = 0
	}
	n := ((decodedSize + 64 + chunk - 1) / chunk * chunk) + chunk
	n = max(n, minTotal)
	n = min(n, maxPart)
	return int(n)
}

// bodyReader is the stable reader identity the yEnc decoder holds for the
// life of a connection: it follows c.reader (which is replaced on a STARTTLS
// upgrade) and records read progress for the body idle janitor.
type bodyReader struct {
	c     *Connection
	reads uint8
}

// progressUpdateStride amortizes the monotonic-clock update across body reads.
const progressUpdateStride = 4

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.c.reader.Read(p)
	if n > 0 {
		b.reads++
		if b.reads >= progressUpdateStride {
			b.c.lastProgressNS.Store(nanotimeNow())
			b.reads = 0
		}
	}
	return n, err
}

// nextBodyWithIdleDeadline uses the shared janitor to break a stalled decode.
func (c *Connection) nextBodyWithIdleDeadline(idle time.Duration) (nntpyenc.BodyResult, error) {
	if idle <= 0 {
		idle = streamBodyTimeout
	}
	// Disable any deadline carried in from earlier on this connection.
	_ = c.conn.SetReadDeadline(time.Time{})

	// Arm the janitor for this decode; the connection itself is registered
	// for its whole lifetime (see createConnection/Close). idleNS=0 on exit
	// disarms.
	c.lastProgressNS.Store(nanotimeNow())
	c.idleNS.Store(int64(idle))
	defer c.idleNS.Store(0)

	res, err := c.bodyDec.Next()
	if err != nil {
		// The janitor sets idleNS to 0 after closing a stalled conn, but
		// the race-free signal is "did we make progress within the
		// deadline?". If not, format as a stall error.
		if nanotimeNow()-c.lastProgressNS.Load() > int64(idle) {
			return res, fmt.Errorf("stream idle for %s: %w", idle, err)
		}
	}
	return res, err
}

// nanotimeNow returns the monotonic clock in nanoseconds. Uses [time.Now]'s
// monotonic reading via Sub(zero): one runtime.nanotime call, no wall-clock
// overhead, no allocation.
var nanotimeEpoch = time.Now() //nolint:gochecknoglobals // process-wide monotonic epoch

func nanotimeNow() int64 {
	return int64(time.Since(nanotimeEpoch))
}

// bodyIdleJanitor sweeps connections currently in nextBodyWithIdleDeadline
// and closes any whose last-progress timestamp is older than their idle
// deadline. One goroutine per process, started lazily on first add().
var bodyIdleJanitor = newBodyJanitor() //nolint:gochecknoglobals // one sweeper goroutine per process

const bodyJanitorInterval = 5 * time.Second

type bodyJanitor struct {
	mu      sync.Mutex
	conns   map[*Connection]struct{}
	started atomic.Bool
}

func newBodyJanitor() *bodyJanitor {
	return &bodyJanitor{conns: make(map[*Connection]struct{})}
}

func (j *bodyJanitor) ensureRunning() {
	if !j.started.CompareAndSwap(false, true) {
		return
	}
	go j.run()
}

func (j *bodyJanitor) add(c *Connection) {
	j.ensureRunning()
	j.mu.Lock()
	j.conns[c] = struct{}{}
	j.mu.Unlock()
}

func (j *bodyJanitor) remove(c *Connection) {
	j.mu.Lock()
	delete(j.conns, c)
	j.mu.Unlock()
}

func (j *bodyJanitor) run() {
	tick := time.NewTicker(bodyJanitorInterval)
	defer tick.Stop()
	for range tick.C {
		j.sweep()
	}
}

// sweep closes every registered connection whose armed body copy has made no
// progress within its idle deadline. Snapshot under the lock and act outside
// it so a slow Close() can't hold up other registrations.
func (j *bodyJanitor) sweep() {
	now := nanotimeNow()
	var stalled []*Connection
	j.mu.Lock()
	for c := range j.conns {
		idle := c.idleNS.Load()
		if idle <= 0 {
			continue
		}
		if now-c.lastProgressNS.Load() > idle {
			stalled = append(stalled, c)
		}
	}
	j.mu.Unlock()
	for _, c := range stalled {
		_ = c.conn.Close() // unblocks the in-flight Read
	}
}

func (c *Connection) readResponseWithDeadline(timeout time.Duration) (Response, error) {
	if timeout <= 0 {
		timeout = streamBodyTimeout
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()
	return c.readResponse()
}

func (c *Connection) readResponseCodeWithDeadline(timeout time.Duration) (int, []byte, error) {
	if timeout <= 0 {
		timeout = streamBodyTimeout
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()
	return c.readResponseCode()
}

// Connection represents an NNTP connection.
type Connection struct {
	username, password, address string
	// pool is the ProviderPool this connection belongs to, set at checkout
	// creation. address alone cannot identify it: two accounts on the same
	// host have distinct pools. Carrying the pointer keeps put/release free
	// of map lookups (and of the allocation an ID string would cost).
	// Every dial goes through getOrCreateFromPool, so this is always set;
	// put and release still nil-check defensively.
	pool   *ProviderPool
	port   int
	conn   net.Conn
	text   *textproto.Reader
	reader *bufio.Reader
	writer *bufio.Writer
	logger zerolog.Logger
	closed atomic.Bool

	// bodyDec decodes complete BODY responses, reading through bodyReader.
	// Created once per connection; it retains a reusable 32KB read buffer.
	// Safe only because the protocol is strictly request/response — the
	// decoder never over-reads past the current response's terminator.
	bodyDec       *nntpyenc.BodyDecoder
	bodyTarget    []byte
	bodyTargetSet bool
	// bodySource supplies caller-owned storage on demand for one response.
	// The owning read installs and clears it; Close must not touch it.
	bodySource BodyBuffer

	// Body-decode idle tracking. lastProgressNS is refreshed by bodyReader
	// while source reads make progress; idleNS is armed by
	// nextBodyWithIdleDeadline and read by the shared janitor goroutine
	// when sweeping for stalls. Stored in monotonic nanoseconds
	// (nanotimeNow). idleNS 0 means this connection isn't currently in a
	// body decode and the janitor should skip it.
	lastProgressNS atomic.Int64
	idleNS         atomic.Int64

	// writeTimeout bounds the next command write instead of the default
	// HandshakeTimeout. ping sets it for the length of its DATE so a health
	// check gets one budget for the whole round trip: without it the write
	// keeps the 10s handshake deadline and a peer that stopped reading
	// blocks the ping far past its own timeout. Only ever touched by the
	// single goroutine that owns the connection (it holds a pool slot and
	// the entry is out of the pool), so no synchronisation is needed.
	writeTimeout time.Duration
}

func (c *Connection) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	bodyIdleJanitor.remove(c)
	return c.conn.Close()
}

func (c *Connection) IsClosed() bool {
	return c.closed.Load()
}

func (c *Connection) authenticate() error {
	// Send AUTHINFO USER command
	if err := c.sendCommandArg("AUTHINFO USER", c.username); err != nil {
		return NewConnectionError(fmt.Errorf("failed to send username: %w", err))
	}

	resp, err := c.readResponse()
	if err != nil {
		return NewConnectionError(fmt.Errorf("failed to read user response: %w", err))
	}

	if resp.Code != codePasswordRequired {
		return classifyNNTPError(resp.Code, fmt.Sprintf("unexpected response to AUTHINFO USER: %s", resp.Message))
	}

	// Send AUTHINFO PASS command
	if sendCommandArgErr := c.sendCommandArg("AUTHINFO PASS", c.password); sendCommandArgErr != nil {
		return NewConnectionError(fmt.Errorf("failed to send password: %w", sendCommandArgErr))
	}

	resp, err = c.readResponse()
	if err != nil {
		return NewConnectionError(fmt.Errorf("failed to read password response: %w", err))
	}

	if resp.Code != codeAuthAccepted {
		return classifyNNTPError(resp.Code, fmt.Sprintf("[%s] authentication failed: %s", c.address, resp.Message))
	}
	return nil
}

// ping sends a simple command to test the connection. timeout bounds the
// whole DATE round trip; <=0 uses PingTimeout. The budget differs by caller:
// a checkout verify-ping is user-visible latency and stays tight, while the
// reaper's background keepalive can afford to wait out congestion.
func (c *Connection) ping(timeout time.Duration) error {
	if c.conn == nil {
		return NewConnectionError(errors.New("connection is nil"))
	}
	if timeout <= 0 {
		timeout = defaultPingTimeout
	}
	_ = c.conn.SetDeadline(time.Now().Add(timeout))
	c.writeTimeout = timeout
	defer func() {
		c.writeTimeout = 0
		_ = c.conn.SetDeadline(time.Time{})
	}()

	if err := c.sendCommand("DATE"); err != nil {
		return NewConnectionError(err)
	}
	resp, err := c.readResponse()
	if err != nil {
		return NewConnectionError(err)
	}
	if resp.Code != codeDate {
		return NewConnectionError(fmt.Errorf("unexpected DATE response: %d %s", resp.Code, resp.Message))
	}
	return nil
}

// sendCommand sends a command to the NNTP server.
func (c *Connection) sendCommand(command string) error {
	return c.sendCommandArg(command, "")
}

func (c *Connection) sendCommandArg(command, arg string) error {
	writeTimeout := c.writeTimeout
	if writeTimeout <= 0 {
		writeTimeout = defaultHandshakeTimeout
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	defer func() { _ = c.conn.SetWriteDeadline(time.Time{}) }()
	if err := c.writeCommandArg(command, arg); err != nil {
		return err
	}
	return c.writer.Flush()
}

func (c *Connection) writeCommandArg(command, arg string) error {
	if _, err := c.writer.WriteString(command); err != nil {
		return err
	}
	if arg != "" {
		if err := c.writer.WriteByte(' '); err != nil {
			return err
		}
		if _, err := c.writer.WriteString(arg); err != nil {
			return err
		}
	}
	if _, err := c.writer.WriteString("\r\n"); err != nil {
		return err
	}
	return nil
}

// readResponse reads a response from the NNTP server.
func (c *Connection) readResponse() (Response, error) {
	code, message, err := c.readResponseCode()
	if err != nil {
		return Response{}, err
	}

	return Response{
		Code:    code,
		Message: string(message),
	}, nil
}

// readResponseCode parses a short NNTP status line in-place from the connection
// buffer. Most BODY callers only need the code on success, so keeping the
// message as bytes avoids materializing a response string for every article.
func (c *Connection) readResponseCode() (int, []byte, error) {
	const statusCodeLen = 3
	line, err := c.reader.ReadSlice('\n')
	if err != nil {
		return 0, nil, err
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	if len(line) < statusCodeLen ||
		line[0] < '0' || line[0] > '9' ||
		line[1] < '0' || line[1] > '9' ||
		line[2] < '0' || line[2] > '9' ||
		(len(line) > statusCodeLen && line[statusCodeLen] != ' ') {
		return 0, nil, fmt.Errorf("invalid response code: %s", line)
	}

	code := int(line[0]-'0')*100 + int(line[1]-'0')*10 + int(line[2]-'0')
	if len(line) == statusCodeLen {
		return code, nil, nil
	}
	return code, line[statusCodeLen+1:], nil
}

// requestBody sends BODY and decodes the complete response through the
// per-connection decoder: status line, yEnc payload, and ".\r\n" terminator
// in one pass, with size and CRC verification. The returned Data buffer
// comes from bodyBufPool and the caller owns it. On error the connection may
// be mid-response and unusable; callers rely on the pool layer to discard
// errored connections.
func (c *Connection) requestBody(messageID string) (nntpyenc.BodyResult, error) {
	return c.requestBodyBuffered(messageID, nil, nil, true)
}

func (c *Connection) requestBodyBuffered(
	messageID string,
	dst []byte,
	source BodyBuffer,
	pooled bool,
) (nntpyenc.BodyResult, error) {
	messageID = FormatMessageID(messageID)
	if err := c.sendCommandArg("BODY", messageID); err != nil {
		return nntpyenc.BodyResult{}, NewConnectionError(fmt.Errorf("failed to send BODY command: %w", err))
	}
	return c.readBodyBuffered(dst, source, pooled)
}

func (c *Connection) readBodyBuffered(dst []byte, source BodyBuffer, pooled bool) (nntpyenc.BodyResult, error) {
	if !pooled {
		if source != nil {
			c.bodySource = source
		} else {
			c.bodyTarget = dst[:0]
			c.bodyTargetSet = true
		}
		defer func() {
			c.bodyTarget = nil
			c.bodyTargetSet = false
			c.bodySource = nil
		}()
	}
	res, err := c.nextBodyWithIdleDeadline(streamBodyTimeout)
	if err != nil {
		if pooled {
			putBodyBuf(res.Data)
		}
		res.Data = nil
		if res.StatusCode == 0 {
			return res, NewConnectionError(fmt.Errorf("failed to read body response: %w", err))
		}
		if errors.Is(err, nntpyenc.ErrDataMissing) {
			// The article exists but its body holds no yEnc data — same
			// taxonomy the segment fetcher applies to zero-byte bodies.
			return res, &Error{Type: ErrorTypeArticleNotFound, Code: res.StatusCode, Message: err.Error()}
		}
		return res, classifyTransferError("streaming yenc decode failed", err)
	}
	if res.StatusCode != codeBodyFollows {
		if pooled {
			putBodyBuf(res.Data)
		}
		res.Data = nil
		return res, classifyNNTPError(res.StatusCode, res.Message)
	}
	return res, nil
}

func (c *Connection) nextBodyBuffer() []byte {
	if c.bodyTargetSet {
		c.bodyTargetSet = false
		return c.bodyTarget[:0]
	}
	if source := c.bodySource; source != nil {
		// Clear before invoking so a panic cannot leave the source installed.
		c.bodySource = nil
		if buf := source.DecodeBuffer(); buf != nil {
			return buf[:0]
		}
		// The read still owes caller-owned storage: connection-pooled
		// scratch must never escape as a retained decoded result.
		return []byte{}
	}
	return getBodyBuf()
}

func metadataFromResult(meta nntpyenc.DecoderMeta) *YencMetadata {
	return &YencMetadata{
		Name:     meta.FileName,
		Size:     meta.FileSize,
		Part:     meta.PartNumber,
		Total:    meta.TotalParts,
		Offset:   meta.Offset,
		PartSize: meta.PartSize,
		Begin:    meta.Begin(),
		End:      meta.End(),
	}
}

// GetBody retrieves article body by message ID as raw bytes (used by GetHeader).
func (c *Connection) GetBody(messageID string) ([]byte, error) {
	messageID = FormatMessageID(messageID)
	if err := c.sendCommandArg("BODY", messageID); err != nil {
		return nil, NewConnectionError(fmt.Errorf("failed to send BODY command: %w", err))
	}

	code, message, err := c.readResponseCodeWithDeadline(streamBodyTimeout)
	if err != nil {
		return nil, NewConnectionError(fmt.Errorf("failed to read body response: %w", err))
	}

	if code != codeBodyFollows {
		return nil, classifyNNTPError(code, string(message))
	}

	// Set read deadline to prevent hanging on stalled servers
	_ = c.conn.SetReadDeadline(time.Now().Add(streamBodyTimeout))
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()

	body, err := c.readDotBytes()
	if err != nil {
		return nil, classifyTransferError("failed to read body", err)
	}
	return body, nil
}

// GetDecodedBody retrieves and decodes an article body in one batch pass.
func (c *Connection) GetDecodedBody(messageID string) ([]byte, error) {
	decoded, _, err := c.GetDecodedBodyWithMetadata(messageID)
	return decoded, err
}

// BodyDestination selects how one pipelined article is delivered. Writer
// takes precedence and receives decoded data from reusable connection-owned
// storage. Otherwise Body is returned using Buffer as caller-owned storage.
type BodyDestination struct {
	Buffer []byte
	Writer io.Writer
	// Skip omits an accepted article while preserving its result index.
	Skip bool
	// BufferSource supplies Buffer on demand. It takes precedence over
	// Buffer and is ignored when Writer is set.
	BufferSource BodyBuffer
}

// BodyBuffer supplies caller-owned decoded storage on demand. The decoder
// asks for it only when it first needs output for a recognized yEnc body,
// so a pending status, a negative status or a header alone never allocates
// it. DecodeBuffer must return the same backing array for repeated calls
// within one logical destination, so that a provider retry cannot change
// the caller's storage identity.
type BodyBuffer interface {
	DecodeBuffer() []byte
}

// DecodedBodyResult is the outcome of one article in a BODY pipeline.
type DecodedBodyResult struct {
	Body  []byte
	Bytes int64
	Error error
}

// PipelineBodies sends multiple BODY commands with one flush and decodes their
// ordered responses. Per-article results preserve partial success. The returned
// error describes the batch-level failure used for retry and provider failover.
// The connection must not be used concurrently.
func (c *Connection) PipelineBodies(messageIDs []string, destinations []BodyDestination) ([]DecodedBodyResult, error) {
	if len(messageIDs) != len(destinations) {
		return nil, fmt.Errorf(
			"BODY pipeline has %d message IDs and %d destinations",
			len(messageIDs),
			len(destinations),
		)
	}
	results := make([]DecodedBodyResult, len(messageIDs))
	if !slices.ContainsFunc(destinations, func(d BodyDestination) bool { return !d.Skip }) {
		return results, nil
	}
	if err := c.writePipeline("BODY", messageIDs, func(i int) bool { return destinations[i].Skip }); err != nil {
		return results, NewConnectionError(err)
	}

	var firstArticleErr error
	for i := range messageIDs {
		if destinations[i].Skip {
			continue
		}
		result, status := c.readPipelinedBody(destinations[i])
		results[i] = result
		if result.Error == nil {
			continue
		}
		err := fmt.Errorf("BODY pipeline article %d/%d: %w", i+1, len(messageIDs), result.Error)
		if status != 0 {
			// Status-line negatives, fully consumed yEnc decode failures, and
			// destination write failures all leave a clean protocol boundary.
			// Drain the rest of the ordered pipeline before returning so the
			// connection remains reusable.
			if firstArticleErr == nil {
				firstArticleErr = err
			}
			continue
		}
		for j := i + 1; j < len(results); j++ {
			if !destinations[j].Skip {
				results[j].Error = err
			}
		}
		return results, err
	}
	return results, firstArticleErr
}

// readPipelinedBody reads the next pipelined BODY response into dest. It
// also returns the parsed status code; 0 means the status line was never
// read and the connection is mid-response.
func (c *Connection) readPipelinedBody(dest BodyDestination) (DecodedBodyResult, int) {
	res, err := c.readBodyBuffered(dest.Buffer, dest.BufferSource, dest.Writer != nil)
	if err != nil {
		return DecodedBodyResult{Error: err}, res.StatusCode
	}
	if dest.Writer == nil {
		return DecodedBodyResult{Body: res.Data, Bytes: int64(len(res.Data))}, res.StatusCode
	}
	n, writeErr := dest.Writer.Write(res.Data)
	if writeErr == nil && n != len(res.Data) {
		writeErr = io.ErrShortWrite
	}
	putBodyBuf(res.Data)
	return DecodedBodyResult{Bytes: int64(n), Error: writeErr}, res.StatusCode
}

// writePipeline writes one command per message ID, except those skip
// reports, under a single write deadline and flush.
func (c *Connection) writePipeline(command string, messageIDs []string, skip func(int) bool) error {
	writeTimeout := c.writeTimeout
	if writeTimeout <= 0 {
		writeTimeout = defaultHandshakeTimeout
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	defer func() { _ = c.conn.SetWriteDeadline(time.Time{}) }()
	for i, messageID := range messageIDs {
		if skip != nil && skip(i) {
			continue
		}
		if err := c.writeCommandArg(command, FormatMessageID(messageID)); err != nil {
			return fmt.Errorf("write %s pipeline at %d/%d: %w", command, i+1, len(messageIDs), err)
		}
	}
	if err := c.writer.Flush(); err != nil {
		return fmt.Errorf("flush %s pipeline: %w", command, err)
	}
	return nil
}

// GetDecodedBodyWithMetadata retrieves and decodes the article body while also
// returning the parsed yEnc metadata from the same pass. The returned slice
// escapes to the caller and is not recycled.
func (c *Connection) GetDecodedBodyWithMetadata(messageID string) ([]byte, *YencMetadata, error) {
	// The result escapes to a long-lived caller, so it must not take a 1 MiB
	// scratch buffer out of bodyBufPool permanently. Let rapidyenc allocate
	// storage sized for this article, just as DecodeBodyInto does when called
	// with an empty destination.
	res, err := c.requestBodyBuffered(messageID, nil, nil, false)
	if err != nil {
		return nil, nil, err
	}
	return res.Data, metadataFromResult(res.Meta), nil
}

// StreamBody decodes one article body and writes it to w in a single Write.
// On the streaming path w is the segment cache, where every Write costs a
// pwrite plus an exclusive buffer-lock acquisition; segment readers only see
// bytes after Finalize, so whole-article batching adds no visible latency.
func (c *Connection) StreamBody(messageID string, w io.Writer) (int64, error) {
	res, err := c.requestBody(messageID)
	if err != nil {
		return 0, err
	}
	n, err := w.Write(res.Data)
	putBodyBuf(res.Data)
	return int64(n), err
}

// DecodeBodyInto verifies one yEnc article into storage supplied by the
// caller. The returned slice belongs to the caller and may be retained.
func (c *Connection) DecodeBodyInto(messageID string, dst []byte) ([]byte, error) {
	res, err := c.requestBodyBuffered(messageID, dst, nil, false)
	if err != nil {
		return nil, err
	}
	return res.Data, nil
}

// DecodeBodyWithBuffer verifies one yEnc article into storage the source
// supplies on demand. The decoder asks for that storage only once it has a
// recognized yEnc body, so a pending or negative status allocates nothing.
// The returned slice belongs to the caller and may be retained.
func (c *Connection) DecodeBodyWithBuffer(messageID string, source BodyBuffer) ([]byte, error) {
	res, err := c.requestBodyBuffered(messageID, nil, source, false)
	if err != nil {
		return nil, err
	}
	return res.Data, nil
}

// readDotBytes reads dot-terminated NNTP data using textproto.DotReader
// This matches Python nntplib's efficient buffered approach.
func (c *Connection) readDotBytes() ([]byte, error) {
	// Use textproto's DotReader which efficiently handles dot-stuffing
	// and terminator detection with optimized buffered reading
	dotReader := c.text.DotReader()

	// Pre-allocate for a typical usenet segment (~750KB).
	buf := bytes.NewBuffer(make([]byte, 0, bodyBufInitialCap))

	// Copy from DotReader to buffer
	_, err := io.Copy(buf, dotReader)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// Stat retrieves article statistics by message ID with proper error classification.
func (c *Connection) Stat(messageID string) (int, string, error) {
	messageID = FormatMessageID(messageID)

	if err := c.sendCommandArg("STAT", messageID); err != nil {
		return 0, "", NewConnectionError(fmt.Errorf("failed to send STAT: %w", err))
	}

	resp, err := c.readResponseWithDeadline(streamBodyTimeout)
	if err != nil {
		return 0, "", NewConnectionError(fmt.Errorf("failed to read STAT response: %w", err))
	}
	return parseStatResponse(resp)
}

// parseStatResponse returns the article number and echoed message ID of a
// "223 n <id>" response.
func parseStatResponse(resp Response) (int, string, error) {
	if resp.Code != codeArticleExists {
		return 0, "", classifyNNTPError(resp.Code, resp.Message)
	}

	const wantFields = 2 // "n <message-id>"
	fields := strings.Fields(resp.Message)
	if len(fields) < wantFields {
		return 0, "", NewProtocolError(resp.Code, fmt.Sprintf("unexpected STAT response format: %q", resp.Message))
	}

	articleNumber, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, "", NewProtocolError(resp.Code, fmt.Sprintf("invalid article number %q: %v", fields[0], err))
	}
	return articleNumber, fields[1], nil
}

// StatBatch pipelines independent STAT commands in one write and consumes the
// ordered single-line responses. A transport failure makes the connection
// unusable and marks the unread suffix with the same error.
func (c *Connection) StatBatch(messageIDs []string) ([]StatResult, error) {
	results := make([]StatResult, len(messageIDs))
	if len(messageIDs) == 0 {
		return results, nil
	}
	for i, messageID := range messageIDs {
		results[i].MessageID = messageID
	}

	if err := c.writePipeline("STAT", messageIDs, nil); err != nil {
		pipelineErr := NewConnectionError(err)
		markStatSuffixError(results, 0, pipelineErr)
		return results, pipelineErr
	}

	for i := range results {
		resp, err := c.readResponseWithDeadline(streamBodyTimeout)
		if err != nil {
			pipelineErr := NewConnectionError(fmt.Errorf("read STAT pipeline at %d/%d: %w", i+1, len(results), err))
			markStatSuffixError(results, i, pipelineErr)
			return results, pipelineErr
		}
		_, _, statErr := parseStatResponse(resp)
		if statErr == nil {
			results[i].Available = true
			continue
		}
		results[i].Error = statErr
	}
	return results, nil
}

func markStatSuffixError(results []StatResult, start int, err error) {
	for i := start; i < len(results); i++ {
		results[i].Available = false
		results[i].Error = err
	}
}

// FormatMessageID ensures message ID has proper format. Message IDs come
// from NZB files, so embedded CR/LF is stripped: it would otherwise end the
// command line and inject further NNTP commands on a pipelined connection.
func FormatMessageID(messageID string) string {
	if strings.ContainsAny(messageID, "\r\n") {
		messageID = strings.NewReplacer("\r", "", "\n", "").Replace(messageID)
	}
	messageID = strings.TrimSpace(messageID)
	if !strings.HasPrefix(messageID, "<") {
		messageID = "<" + messageID
	}
	if !strings.HasSuffix(messageID, ">") {
		messageID += ">"
	}
	return messageID
}
