package nntp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func pipelineSkipServer(t *testing.T, c *Connection, server net.Conn, fn func(*bufio.Reader) error) <-chan error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	_ = c.conn.SetDeadline(deadline)
	_ = server.SetDeadline(deadline)
	done := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Go(func() { done <- fn(bufio.NewReader(server)) })
	t.Cleanup(func() {
		_ = c.conn.Close()
		_ = server.Close()
		workers.Wait()
	})
	return done
}

func readPipelineSkipCommands(reader *bufio.Reader, ids []string) error {
	for _, id := range ids {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if line != "BODY "+id+"\r\n" {
			return fmt.Errorf("command = %q, want BODY %s", line, id)
		}
	}
	return nil
}

func finishPipelineSkipStat(reader *bufio.Reader, server net.Conn) error {
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if line != "STAT <after@skip>\r\n" {
		return fmt.Errorf("after-pipeline command = %q", line)
	}
	_, err = io.WriteString(server, "223 0 <after@skip>\r\n")
	return err
}

func TestPipelineBodiesSkipPositions(t *testing.T) {
	t.Parallel()
	for _, mask := range []int{0, 1, 2, 4, 8, 5, 10, 15} {
		t.Run(fmt.Sprintf("%04b", mask), func(t *testing.T) {
			t.Parallel()
			testPipelineSkipMask(t, mask)
		})
	}
}

func testPipelineSkipMask(t *testing.T, mask int) {
	t.Helper()
	c, server := newBodyTestConn(t)
	ids := []string{"<zero@skip>", "<one@skip>", "<two@skip>", "<three@skip>"}
	destinations := make([]BodyDestination, len(ids))
	payloads := make([][]byte, len(ids))
	var pending []string
	for i := range ids {
		payloads[i] = testPayload(4096 + 137*i)
		destinations[i] = BodyDestination{Buffer: bytes.Repeat([]byte{0xa5}, 8192), Skip: mask&(1<<i) != 0}
		if !destinations[i].Skip {
			pending = append(pending, ids[i])
		}
	}
	done := pipelineSkipServer(t, c, server, func(reader *bufio.Reader) error {
		if err := readPipelineSkipCommands(reader, pending); err != nil {
			return err
		}
		if err := writeSkipResponses(server, ids, destinations, payloads); err != nil {
			return err
		}
		return finishPipelineSkipStat(reader, server)
	})
	results, err := c.PipelineBodies(ids, destinations)
	if err != nil || len(results) != len(ids) {
		t.Fatalf("results length=%d, error=%v", len(results), err)
	}
	for i, result := range results {
		checkSkipResult(t, i, result, destinations[i], payloads[i])
	}
	if _, _, statErr := c.Stat("<after@skip>"); statErr != nil {
		t.Fatalf("connection reuse: %v", statErr)
	}
	if doneErr := <-done; doneErr != nil {
		t.Fatal(doneErr)
	}
}

// writeSkipResponses answers every non-skipped BODY with its payload.
func writeSkipResponses(server io.Writer, ids []string, destinations []BodyDestination, payloads [][]byte) error {
	for i, id := range ids {
		if destinations[i].Skip {
			continue
		}
		if _, err := fmt.Fprintf(server, "222 0 %s body\r\n%s.\r\n", id, encodeBody(payloads[i])); err != nil {
			return err
		}
	}
	return nil
}

func checkSkipResult(t *testing.T, i int, result DecodedBodyResult, destination BodyDestination, payload []byte) {
	t.Helper()
	if !destination.Skip {
		if result.Error != nil || !bytes.Equal(result.Body, payload) {
			t.Errorf("pending result %d = (%d bytes, %v)", i, len(result.Body), result.Error)
		}
		return
	}
	if result.Body != nil || result.Bytes != 0 || result.Error != nil {
		t.Errorf("skipped result %d = %+v", i, result)
	}
	if !bytes.Equal(destination.Buffer, bytes.Repeat([]byte{0xa5}, 8192)) {
		t.Errorf("skipped storage %d changed", i)
	}
}

func TestPipelineBodiesAllSkippedNeedsNoConnection(t *testing.T) {
	t.Parallel()
	var writer bytes.Buffer
	results, err := (&Connection{}).PipelineBodies(
		[]string{"<skip@all>"},
		[]BodyDestination{{Writer: &writer, Skip: true}},
	)
	if err != nil || len(results) != 1 || results[0].Body != nil || results[0].Bytes != 0 || results[0].Error != nil ||
		writer.Len() != 0 {
		t.Fatalf("all skipped result=%+v, error=%v, writer bytes=%d", results, err, writer.Len())
	}
}

func TestPipelineBodiesSkipPreservesErrorIndices(t *testing.T) {
	t.Parallel()
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprintf("disconnect=%t", disconnect), func(t *testing.T) {
			t.Parallel()
			testPipelineSkipErrorIndices(t, disconnect)
		})
	}
}

func testPipelineSkipErrorIndices(t *testing.T, disconnect bool) {
	t.Helper()
	c, server := newBodyTestConn(t)
	ids := []string{"<zero@skip>", "<one@skip>", "<two@skip>", "<three@skip>"}
	destinations := []BodyDestination{{Skip: true}, {}, {Skip: true}, {}}
	payload := testPayload(8192)
	done := pipelineSkipServer(t, c, server, func(reader *bufio.Reader) error {
		if err := readPipelineSkipCommands(reader, []string{ids[1], ids[3]}); err != nil {
			return err
		}
		if disconnect {
			return server.Close()
		}
		if _, err := fmt.Fprintf(
			server,
			"430 missing\r\n222 0 %s body\r\n%s.\r\n",
			ids[3],
			encodeBody(payload),
		); err != nil {
			return err
		}
		return finishPipelineSkipStat(reader, server)
	})
	results, err := c.PipelineBodies(ids, destinations)
	if err == nil || !strings.Contains(err.Error(), "BODY pipeline article 2/4:") {
		t.Fatalf("batch error = %v", err)
	}
	wantType := ErrorTypeArticleNotFound
	if disconnect {
		wantType = ErrorTypeConnection
	}
	if typed, ok := errors.AsType[*Error](err); !ok || typed.Type != wantType {
		t.Fatalf("error type = %v, want %v", err, wantType)
	}
	if !errors.Is(err, results[1].Error) {
		t.Fatalf("batch lost failed slot error: %v / %v", err, results[1].Error)
	}
	for _, i := range []int{0, 2} {
		if results[i].Error != nil || results[i].Body != nil || results[i].Bytes != 0 {
			t.Errorf("skipped slot %d acquired result %+v", i, results[i])
		}
	}
	checkSkipTail(t, c, results[3], err, disconnect, payload)
	if doneErr := <-done; doneErr != nil {
		t.Fatal(doneErr)
	}
}

// checkSkipTail verifies the slot after a failed one: it inherits the batch
// error on disconnect, and is drained and decoded otherwise.
func checkSkipTail(
	t *testing.T,
	c *Connection,
	last DecodedBodyResult,
	batchErr error,
	disconnect bool,
	payload []byte,
) {
	t.Helper()
	if disconnect {
		if !errors.Is(last.Error, batchErr) {
			t.Fatalf("unattempted slot lost batch error: %v", last.Error)
		}
		return
	}
	if last.Error != nil || !bytes.Equal(last.Body, payload) {
		t.Fatal("later response was not drained and decoded")
	}
	if _, _, statErr := c.Stat("<after@skip>"); statErr != nil {
		t.Fatalf("connection reuse: %v", statErr)
	}
}
