package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/sirrobot01/decypharr/internal/config"
)

// Log file rotation policy.
const (
	logMaxSizeMB  = 10
	logMaxAgeDays = 15
	logMaxBackups = 10
)

// defaultLogger is the process-wide logger returned by Default.
var defaultLogger = sync.OnceValue(func() zerolog.Logger { return New("decypharr") })

// sharedRotatingLogFile returns the process-wide lumberjack writer. All
// component loggers share one rotator so they don't race on the same file
// (each *lumberjack.Logger runs its own mill goroutine and rotation cycle).

var sharedRotatingLogFile = sync.OnceValue(func() *lumberjack.Logger {
	return &lumberjack.Logger{
		Filename:   filepath.Join(GetLogPath(), "decypharr.log"),
		MaxSize:    logMaxSizeMB,
		MaxAge:     logMaxAgeDays,
		MaxBackups: logMaxBackups,
		Compress:   true,
	}
})

// GetLogPath returns <config dir>/logs, creating it if needed.
func GetLogPath() string {
	logsDir := filepath.Join(config.GetMainPath(), "logs")

	if _, err := os.Stat(logsDir); os.IsNotExist(err) {
		if mkdirAllErr := os.MkdirAll(logsDir, 0o750); mkdirAllErr != nil {
			panic(fmt.Sprintf("Failed to create logs directory: %v", mkdirAllErr))
		}
	}

	return logsDir
}

// New returns a logger that tags messages with prefix and writes to stdout
// and the rotating log file at the configured level.
func New(prefix string) zerolog.Logger {
	level := config.Get().LogLevel

	logFile := sharedRotatingLogFile()

	consoleWriter := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: "2006-01-02 15:04:05",
		NoColor:    false, // Set to true if you don't want colors
		FormatLevel: func(i any) string {
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
		},
		FormatMessage: func(i any) string {
			return fmt.Sprintf("[%s] %v", prefix, i)
		},
	}

	fileWriter := zerolog.ConsoleWriter{
		Out:        logFile,
		TimeFormat: "2006-01-02 15:04:05",
		NoColor:    true, // No colors in file output
		FormatLevel: func(i any) string {
			return strings.ToUpper(fmt.Sprintf("| %-6s|", i))
		},
		FormatMessage: func(i any) string {
			return fmt.Sprintf("[%s] %v", prefix, i)
		},
	}

	multi := zerolog.MultiLevelWriter(consoleWriter, fileWriter)

	l := zerolog.New(multi).
		With().
		Timestamp().
		Logger().
		Level(zerolog.InfoLevel)

	// Set the log level
	level = strings.ToLower(level)
	switch level {
	case "debug":
		l = l.Level(zerolog.DebugLevel)
	case "info":
		l = l.Level(zerolog.InfoLevel)
	case "warn":
		l = l.Level(zerolog.WarnLevel)
	case "error":
		l = l.Level(zerolog.ErrorLevel)
	case "trace":
		l = l.Level(zerolog.TraceLevel)
	}
	return l
}

// Default returns the shared "decypharr" logger.
func Default() zerolog.Logger {
	return defaultLogger()
}
