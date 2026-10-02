package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/manifest"
	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
)

const (
	exitError = 1
	exitUsage = 2

	defaultMaxConcurrent = 10
	ruleWidth            = 80
	bytesPerMB           = 1024 * 1024
	bytesPerGB           = 1024 * bytesPerMB
)

var errUsage = errors.New("usage")

func main() {
	output := zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}
	log := zerolog.New(output).With().Timestamp().Logger()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)

	err := run(os.Args[1:], os.Stdout, log)
	switch {
	case errors.Is(err, errUsage):
		os.Exit(exitUsage)
	case err != nil:
		log.Error().Err(err).Msg("Parser test failed")
		os.Exit(exitError)
	}
}

func run(args []string, w io.Writer, log zerolog.Logger) error {
	fs := flag.NewFlagSet("test-parser", flag.ContinueOnError)
	localOnly := fs.Bool("local-only", false, "decode the local NZB manifest without connecting to NNTP")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), "Usage: test-parser [-local-only] <nzb-file>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errUsage
	}

	nzbFile := fs.Arg(0)
	content, err := os.ReadFile(nzbFile)
	if err != nil {
		return fmt.Errorf("read NZB file %s: %w", nzbFile, err)
	}
	if *localOnly {
		started := time.Now()
		decoded, decodeErr := manifest.Decode(bytes.NewReader(content))
		if decodeErr != nil {
			return fmt.Errorf("decode local NZB manifest %s: %w", nzbFile, decodeErr)
		}
		printManifestSummary(w, nzbFile, decoded, time.Since(started))
		return nil
	}
	return parseAndProcess(w, log, nzbFile, content)
}

func parseAndProcess(w io.Writer, log zerolog.Logger, nzbFile string, content []byte) error {
	config.SetConfigPath("data/")
	cfg := config.Get()
	client, err := nntp.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("create NNTP client: %w", err)
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			log.Warn().Err(closeErr).Msg("Failed to close NNTP client")
		}
	}()

	maxConcurrent := cfg.Usenet.ProcessingMaxConnections
	if maxConcurrent <= 0 {
		maxConcurrent = cfg.Usenet.MaxConnections
	}
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxConcurrent
	}
	p := parser.NewParser(client, maxConcurrent, log)
	parseStarted := time.Now()
	nzb, groups, err := p.Parse(context.Background(), nzbFile, content)
	if err != nil {
		return fmt.Errorf("parse NZB: %w", err)
	}
	parseElapsed := time.Since(parseStarted)
	processStarted := time.Now()
	nzb, err = p.Process(context.Background(), nzb, groups)
	if err != nil {
		return fmt.Errorf("process NZB: %w", err)
	}
	processElapsed := time.Since(processStarted)

	printFileSummary(w, nzb, parseElapsed, processElapsed)
	printMetrics(w, p.Metrics())
	log.Info().Msg("Parser test completed successfully")
	return nil
}

func printFileSummary(w io.Writer, nzb *storage.NZB, parseElapsed, processElapsed time.Duration) {
	_, _ = fmt.Fprintln(w, strings.Repeat("=", ruleWidth))
	_, _ = fmt.Fprintln(w, "FILE SUMMARY")
	_, _ = fmt.Fprintln(w, strings.Repeat("=", ruleWidth))
	_, _ = fmt.Fprintf(w, "NZB ID:        %s\n", nzb.ID)
	_, _ = fmt.Fprintf(w, "Name:          %s\n", nzb.Name)
	_, _ = fmt.Fprintf(w, "Total Size:    %.2f GB\n", float64(nzb.TotalSize)/bytesPerGB)
	_, _ = fmt.Fprintf(w, "Logical Files: %d\n", len(nzb.Files))
	_, _ = fmt.Fprintf(w, "Parse Phase:   %s\n", parseElapsed.Round(time.Microsecond))
	_, _ = fmt.Fprintf(w, "Process Phase: %s\n", processElapsed.Round(time.Microsecond))

	for i, file := range nzb.Files {
		_, _ = fmt.Fprintf(w, "\n[%d] %s\n", i+1, file.Name)
		_, _ = fmt.Fprintf(w, "    Size:         %.2f MB (%d bytes)\n", float64(file.Size)/bytesPerMB, file.Size)
		_, _ = fmt.Fprintf(w, "    Segments:     %d\n", len(file.Segments))
		_, _ = fmt.Fprintf(w, "    Password:     %s\n", passwordStatus(file.Password))
		if file.InternalPath != "" {
			_, _ = fmt.Fprintf(w, "    Internal:     %s\n", file.InternalPath)
		}
		if file.IsStored {
			_, _ = fmt.Fprintln(w, "    Compression:  Stored (seekable)")
		} else {
			_, _ = fmt.Fprintln(w, "    Compression:  Compressed")
		}

		zeroBytes := 0
		for _, segment := range file.Segments {
			if segment.Bytes <= 0 {
				zeroBytes++
			}
		}
		if zeroBytes > 0 {
			_, _ = fmt.Fprintf(w, "    Zero-byte segments: %d\n", zeroBytes)
		}
	}
}

func printMetrics(w io.Writer, metrics parser.ArticleMetrics) {
	_, _ = fmt.Fprintf(w, "\nAnalyzer article traffic\n")
	_, _ = fmt.Fprintf(w, "    Header requests: %d\n", metrics.HeaderRequests)
	_, _ = fmt.Fprintf(w, "    Body requests:   %d\n", metrics.BodyRequests)
	_, _ = fmt.Fprintf(w, "    STAT requests:   %d\n", metrics.StatRequests)
	_, _ = fmt.Fprintf(w, "    Network BODY:    %d\n", metrics.NetworkBodies)
	_, _ = fmt.Fprintf(w, "    Network STAT:    %d\n", metrics.NetworkStats)
	_, _ = fmt.Fprintf(w, "    Cache hits:      %d\n", metrics.CacheHits)
	_, _ = fmt.Fprintf(w, "    Shared loads:    %d\n", metrics.SharedLoads)
	_, _ = fmt.Fprintf(w, "    Bytes fetched:   %d\n", metrics.BytesFetched)
	_, _ = fmt.Fprintf(w, "    Cached bodies:   %d (%d bytes)\n", metrics.CachedBodies, metrics.CachedBodyBytes)
	_, _ = fmt.Fprintf(w, "    Cached entries:  %d\n", metrics.CachedEntries)
}

func printManifestSummary(w io.Writer, filename string, decoded *manifest.Manifest, elapsed time.Duration) {
	_, _ = fmt.Fprintln(w, strings.Repeat("=", ruleWidth))
	_, _ = fmt.Fprintln(w, "LOCAL MANIFEST SUMMARY")
	_, _ = fmt.Fprintln(w, strings.Repeat("=", ruleWidth))
	_, _ = fmt.Fprintf(w, "File:              %s\n", filename)
	_, _ = fmt.Fprintf(w, "Posted Files:      %d\n", len(decoded.Files))
	_, _ = fmt.Fprintf(w, "Available Segments: %d\n", decoded.Stats.AvailableSegments)
	_, _ = fmt.Fprintf(w, "Total Segments:    %d\n", decoded.Stats.TotalSegments)
	_, _ = fmt.Fprintf(w, "Reported Bytes:    %d\n", decoded.Stats.Bytes)
	_, _ = fmt.Fprintf(w, "Decode Time:       %s\n", elapsed.Round(time.Microsecond))
}

func passwordStatus(password string) string {
	if password == "" {
		return "None"
	}
	return "Protected (***)"
}
