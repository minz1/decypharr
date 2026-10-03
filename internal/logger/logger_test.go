package logger_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/logger"
)

func TestFactoryWritesPrefixedLinesAtItsLevel(t *testing.T) {
	t.Parallel()
	var file bytes.Buffer
	log := logger.NewFactory("warn", nil, &file).New("unit")
	log.Info().Msg("hidden")
	log.Warn().Msg("shown")
	out := file.String()
	if strings.Contains(out, "hidden") {
		t.Fatalf("info line written at warn level: %q", out)
	}
	if !strings.Contains(out, "[unit] shown") || !strings.Contains(out, "WARN") {
		t.Fatalf("warn line missing or unprefixed: %q", out)
	}
}

func TestParseLevelDefaultsToInfo(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]zerolog.Level{
		"debug": zerolog.DebugLevel, "TRACE": zerolog.TraceLevel, "error": zerolog.ErrorLevel,
		"": zerolog.InfoLevel, "bogus": zerolog.InfoLevel,
	} {
		if got := logger.ParseLevel(name); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestDiscardFactoryLogsNothing(t *testing.T) {
	t.Parallel()
	log := logger.Discard().New("unit")
	if log.GetLevel() != zerolog.Disabled {
		t.Fatalf("discard logger level = %v", log.GetLevel())
	}
}

func TestOpenRotatingFileCreatesLogsDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rotator, err := logger.OpenRotatingFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rotator.Close() })
	if rotator.Filename != filepath.Join(dir, "logs", logger.FileName) {
		t.Fatalf("Filename = %q", rotator.Filename)
	}
	if info, statErr := os.Stat(logger.Dir(dir)); statErr != nil || !info.IsDir() {
		t.Fatalf("logs dir not created: %v", statErr)
	}
}
