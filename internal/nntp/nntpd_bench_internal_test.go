package nntp

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
)

const benchSegmentSize = 750 * 1024

func newBenchServerClient(b *testing.B, cfg nntpd.Config, maxConns int) (*nntpd.Server, *Client) {
	b.Helper()
	srv, err := nntpd.New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(srv.Close)

	host, port := srv.Addr()
	client, err := NewClient(&config.Config{
		Usenet: config.Usenet{
			Providers: []config.UsenetProvider{{
				Host:           host,
				Port:           port,
				MaxConnections: maxConns,
			}},
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = client.Close() })
	return srv, client
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// BenchmarkStreamBodyE2E measures one full BODY round trip — command, status
// line, yEnc decode, single write — through a real dialed connection, at
// several simulated RTTs. Per-article cost is 1 RTT + transfer + decode.
func BenchmarkStreamBodyE2E(b *testing.B) {
	payload := nntpd.Pattern(0, benchSegmentSize)
	body := nntpd.Encode(payload, "bench.bin", 1, benchSegmentSize, 0)

	for _, rtt := range []time.Duration{0, 10 * time.Millisecond, 30 * time.Millisecond} {
		b.Run(fmt.Sprintf("rtt%dms", rtt/time.Millisecond), func(b *testing.B) {
			srv, client := newBenchServerClient(b, nntpd.Config{RTT: rtt}, 2)
			srv.AddArticle("<bench@nntpd>", body)

			ctx := context.Background()
			w := &countingWriter{}
			b.SetBytes(benchSegmentSize)
			var iterations int64
			for b.Loop() {
				err := client.ExecuteWithFailover(ctx, WorkloadStreamDemand, func(conn *Connection) error {
					_, err := conn.StreamBody("<bench@nntpd>", w)
					return err
				})
				if err != nil {
					b.Fatal(err)
				}
				iterations++
			}
			if w.n != iterations*benchSegmentSize {
				b.Fatalf("streamed %d bytes, want %d", w.n, iterations*benchSegmentSize)
			}
		})
	}
}

// BenchmarkStatBatchE2E compares the former request/response loop with a
// sixteen-command pipeline on one warm connection. The fake server charges
// one configured RTT per wire burst, so both paths still exercise real TCP
// framing and response parsing.
func BenchmarkStatBatchE2E(b *testing.B) {
	for _, rtt := range []time.Duration{0, 10 * time.Millisecond, 30 * time.Millisecond} {
		for _, pipelined := range []bool{false, true} {
			name := "sequential"
			if pipelined {
				name = "pipelined"
			}
			b.Run(fmt.Sprintf("rtt%dms/%s", rtt/time.Millisecond, name), func(b *testing.B) {
				benchStatBatch(b, rtt, pipelined)
			})
		}
	}
}

func benchStatBatch(b *testing.B, rtt time.Duration, pipelined bool) {
	const pipelineDepth = 16
	srv, client := newBenchServerClient(b, nntpd.Config{RTT: rtt}, 1)
	messageIDs := make([]string, pipelineDepth)
	for i := range pipelineDepth {
		messageIDs[i] = fmt.Sprintf("<stat-%d@nntpd>", i)
		srv.AddArticle(messageIDs[i], []byte{1})
	}
	conn, provider, err := client.getConnectionFromProvider(
		context.Background(),
		WorkloadBackground,
		client.providers[0],
	)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { client.returnOrReleaseConn(conn, provider) })
	b.ReportMetric(pipelineDepth, "stats/op")

	for b.Loop() {
		if statErr := statAll(conn, messageIDs, pipelined); statErr != nil {
			b.Fatal(statErr)
		}
	}
}

func statAll(conn *Connection, messageIDs []string, pipelined bool) error {
	if pipelined {
		_, err := conn.StatBatch(messageIDs)
		return err
	}
	for _, messageID := range messageIDs {
		if _, _, err := conn.Stat(messageID); err != nil {
			return err
		}
	}
	return nil
}

// BenchmarkStreamBodyPriorityUnderDownloadPressure exercises the complete
// BODY path through real TCP connections while bulk downloads saturate every
// provider slot. It measures the time until a decoded segment reaches the
// cache boundary, not merely the scheduler handoff.
func BenchmarkStreamBodyPriorityUnderDownloadPressure(b *testing.B) {
	for _, workload := range []Workload{WorkloadStreamDemand, WorkloadStreamPrefetch, WorkloadDownload} {
		b.Run(workload.String(), func(b *testing.B) { benchStreamPriority(b, workload) })
	}
}

// startDownloadLoad keeps workers busy streaming the article as bulk
// downloads until ctx is canceled. Failures are reported on errs.
func startDownloadLoad(ctx context.Context, client *Client, workers int, errs chan<- error) *sync.WaitGroup {
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for ctx.Err() == nil {
				err := client.ExecuteWithFailover(ctx, WorkloadDownload, func(conn *Connection) error {
					_, err := conn.StreamBody("<priority@nntpd>", io.Discard)
					return err
				})
				if err != nil && ctx.Err() == nil {
					errs <- err
					return
				}
			}
		})
	}
	return &wg
}

func benchStreamPriority(b *testing.B, workload Workload) {
	const (
		slots             = 4
		downloadWorkers   = 12
		providerBandwidth = 32 << 20
		nsPerMilli        = 1e6
	)
	payload := nntpd.Pattern(0, benchSegmentSize)
	body := nntpd.Encode(payload, "priority.bin", 1, benchSegmentSize, 0)
	srv, client := newBenchServerClient(b, nntpd.Config{
		RTT:       10 * time.Millisecond,
		Bandwidth: providerBandwidth,
	}, slots)
	srv.AddArticle("<priority@nntpd>", body)
	pp := client.orderedPools[0]

	loadCtx, cancelLoad := context.WithCancel(context.Background())
	loadErrs := make(chan error, downloadWorkers)
	loadWG := startDownloadLoad(loadCtx, client, downloadWorkers, loadErrs)
	waitForBenchSaturation(b, client, pp, WorkloadDownload, downloadWorkers-slots)

	var totalWait, maxWait, totalSegment time.Duration
	var iterations int64
	b.SetBytes(benchSegmentSize)
	for b.Loop() {
		started := time.Now()
		conn, provider, err := client.getAnyAvailableConnection(context.Background(), workload, providerExclusions{})
		if err != nil {
			b.Fatal(err)
		}
		wait := time.Since(started)
		totalWait += wait
		maxWait = max(maxWait, wait)
		if _, streamBodyErr := conn.StreamBody("<priority@nntpd>", io.Discard); streamBodyErr != nil {
			client.release(conn)
			b.Fatal(streamBodyErr)
		}
		client.put(conn, provider)
		totalSegment += time.Since(started)
		iterations++
	}

	cancelLoad()
	loadWG.Wait()
	close(loadErrs)
	for err := range loadErrs {
		b.Error(err)
	}
	b.ReportMetric(float64(totalWait)/float64(iterations)/nsPerMilli, "mean-admission-ms")
	b.ReportMetric(float64(maxWait)/nsPerMilli, "max-admission-ms")
	b.ReportMetric(float64(totalSegment)/float64(iterations)/nsPerMilli, "mean-segment-ms")
}

// BenchmarkBodyPipelineDepthE2E evaluates the RTT savings and scheduling
// boundary cost of candidate BODY pipeline depths. Every operation retrieves
// four complete 750 KiB articles.
func BenchmarkBodyPipelineDepthE2E(b *testing.B) {
	payload := nntpd.Pattern(0, benchSegmentSize)
	body := nntpd.Encode(payload, "pipeline.bin", 1, benchSegmentSize, 0)
	for _, rtt := range []time.Duration{10 * time.Millisecond, 30 * time.Millisecond} {
		for _, depth := range []int{1, 2, 4} {
			b.Run(fmt.Sprintf("rtt%dms/depth%d", rtt/time.Millisecond, depth), func(b *testing.B) {
				benchBodyPipelineDepth(b, rtt, depth, body)
			})
		}
	}
}

func benchBodyPipelineDepth(b *testing.B, rtt time.Duration, depth int, body []byte) {
	const bodiesPerOperation = 4
	srv, client := newBenchServerClient(b, nntpd.Config{RTT: rtt}, 1)
	messageIDs := make([]string, bodiesPerOperation)
	for i := range bodiesPerOperation {
		messageIDs[i] = fmt.Sprintf("<pipeline-%d@nntpd>", i)
		srv.AddArticle(messageIDs[i], body)
	}
	destinations := make([]BodyDestination, bodiesPerOperation)
	for i := range destinations {
		destinations[i].Buffer = make([]byte, 0, DecodedBodyCapacity(benchSegmentSize))
	}
	conn, provider, err := client.getConnectionFromProvider(
		context.Background(),
		WorkloadStreamPrefetch,
		client.providers[0],
	)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { client.returnOrReleaseConn(conn, provider) })
	b.SetBytes(bodiesPerOperation * benchSegmentSize)
	b.ReportMetric(bodiesPerOperation, "bodies/op")

	for b.Loop() {
		for start := 0; start < len(messageIDs); start += depth {
			end := min(start+depth, len(messageIDs))
			if _, pipelineErr := conn.PipelineBodies(
				messageIDs[start:end],
				destinations[start:end],
			); pipelineErr != nil {
				b.Fatal(pipelineErr)
			}
		}
	}
}
