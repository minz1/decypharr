package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/cmd/decypharr"
	"github.com/sirrobot01/decypharr/internal/task"
)

// pprofReadHeaderTimeout bounds slow clients on the opt-in pprof listener.
const pprofReadHeaderTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("FATAL: Recovered from panic in main: %v\n", r)
			debug.PrintStack()
		}
	}()

	var configPath string
	var pprofAddr string

	// Create a default config directory if it doesn't exist
	flag.StringVar(&configPath, "config", "", "path to the data folder")
	flag.StringVar(&pprofAddr, "pprof", ":6060", "pprof server address (set to empty to disable)")
	flag.Parse()

	// get enable pprof flag from environment variable if not set via flag
	enablePprof := os.Getenv("ENABLE_PPROF") != ""

	if configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		configPath = filepath.Join(home, ".decypharr")
	}

	// Buffer pools are owned by their subsystems: the DFS cache (vfs.NewCache)
	// and the usenet reader each create a buffer.Pool with their own configured
	// RAM budget and disk limit.

	// Create a context canceled on SIGINT/SIGTERM
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := zerolog.New(os.Stderr).With().Timestamp().Logger()
	proc := task.NewRoot(context.WithoutCancel(ctx), "process", logger)
	defer func() {
		proc.Close()
		<-proc.Done()
	}()

	// Start pprof server if enabled
	if pprofAddr != "" && enablePprof {
		if err := proc.Go("pprof", func(ctx context.Context) error {
			if err := servePprof(ctx, proc, pprofAddr); err != nil {
				log.Printf("pprof server error: %v", err)
			}
			return nil
		}); err != nil {
			return err
		}
	}

	return decypharr.Start(ctx, configPath)
}

// servePprof exposes the profiling endpoints on their own mux, so they are
// only reachable on the opt-in pprof listener.
func servePprof(ctx context.Context, g *task.Group, addr string) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("Starting pprof server on %s", addr)
	return servePprofOn(ctx, g, ln)
}

// servePprofOn serves the profiling endpoints on ln, on a child of g, until
// ctx is done, then shuts the server down and returns nil.
func servePprofOn(ctx context.Context, g *task.Group, ln net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: pprofReadHeaderTimeout}

	served := make(chan error, 1)
	serve := g.Child("pprof-serve")
	defer func() {
		serve.Close()
		<-serve.Done()
	}()
	if err := serve.Go("serve", func(context.Context) error {
		served <- srv.Serve(ln)
		return nil
	}); err != nil {
		_ = ln.Close()
		return err
	}

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := task.Detached(ctx, time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
	}
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
