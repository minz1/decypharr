package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Log file rotation policy.
const (
	logMaxSizeMB  = 10
	logMaxAgeDays = 15
	logMaxBackups = 10
)

// FileName is the main log file inside the logs directory.
const FileName = "decypharr.log"

// Dir returns the logs directory of the data folder configDir.
func Dir(configDir string) string {
	return filepath.Join(configDir, "logs")
}

// OpenRotatingFile creates the logs directory of configDir and returns the
// rotating writer for its main log file. The caller owns it: every Factory
// writing to the same file must share it (each *lumberjack.Logger runs its
// own rotation), and the caller closes it at exit.
func OpenRotatingFile(configDir string) (*lumberjack.Logger, error) {
	logsDir := Dir(configDir)
	if err := os.MkdirAll(logsDir, 0o750); err != nil {
		return nil, fmt.Errorf("create logs directory: %w", err)
	}
	return &lumberjack.Logger{
		Filename:   filepath.Join(logsDir, FileName),
		MaxSize:    logMaxSizeMB,
		MaxAge:     logMaxAgeDays,
		MaxBackups: logMaxBackups,
		Compress:   true,
	}, nil
}

// Factory builds component loggers that share one level, console and file.
// A nil *Factory builds disabled loggers, which suits tests.
type Factory struct {
	level   zerolog.Level
	console io.Writer
	file    io.Writer
}

// NewFactory returns a Factory logging at level (debug, info, warn, error or
// trace; anything else means info) to console with colors and to file
// without. Either writer may be nil to skip it.
func NewFactory(level string, console, file io.Writer) *Factory {
	return &Factory{level: ParseLevel(level), console: console, file: file}
}

// Discard returns a Factory whose loggers write nothing.
func Discard() *Factory { return nil }

// ParseLevel maps a configured level name to a zerolog level; unknown names
// mean info.
func ParseLevel(level string) zerolog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return zerolog.DebugLevel
	case "warn":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	case "trace":
		return zerolog.TraceLevel
	default:
		return zerolog.InfoLevel
	}
}

// New returns a logger that tags messages with prefix.
func (f *Factory) New(prefix string) zerolog.Logger {
	if f == nil {
		return zerolog.Nop()
	}
	formatMessage := func(i any) string {
		return fmt.Sprintf("[%s] %v", prefix, i)
	}
	var writers []io.Writer
	if f.console != nil {
		writers = append(writers, zerolog.ConsoleWriter{
			Out:           f.console,
			TimeFormat:    "2006-01-02 15:04:05",
			FormatLevel:   colorLevel,
			FormatMessage: formatMessage,
		})
	}
	if f.file != nil {
		writers = append(writers, zerolog.ConsoleWriter{
			Out:        f.file,
			TimeFormat: "2006-01-02 15:04:05",
			NoColor:    true, // No colors in file output
			FormatLevel: func(i any) string {
				return strings.ToUpper(fmt.Sprintf("| %-6s|", i))
			},
			FormatMessage: formatMessage,
		})
	}
	return zerolog.New(zerolog.MultiLevelWriter(writers...)).
		With().
		Timestamp().
		Logger().
		Level(f.level)
}

// colorLevel renders the level column of console output.
func colorLevel(i any) string {
	var colorCode string
	switch strings.ToLower(fmt.Sprintf("%s", i)) {
	case "debug":
		colorCode = "\033[36m"
	case "info":
		colorCode = "\033[32m"
	case "warn":
		colorCode = "\033[33m"
	case "error":
		colorCode = "\033[31m"
	case "fatal":
		colorCode = "\033[35m"
	case "panic":
		colorCode = "\033[41m"
	default:
		colorCode = "\033[37m" // White
	}
	return fmt.Sprintf("%s| %-6s|\033[0m", colorCode, strings.ToUpper(fmt.Sprintf("%s", i)))
}
