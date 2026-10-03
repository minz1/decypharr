package decypharr

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs"
	"github.com/sirrobot01/decypharr/pkg/mount/external"
	"github.com/sirrobot01/decypharr/pkg/mount/rclone"
	"github.com/sirrobot01/decypharr/pkg/server"
	"github.com/sirrobot01/decypharr/pkg/server/webdav"
	"github.com/sirrobot01/decypharr/pkg/share"
	"github.com/sirrobot01/decypharr/pkg/version"
)

// Start runs decypharr from the data folder dataDir until ctx is cancelled or
// a service fails. Each generation loads the configuration and builds its own
// object graph; a requested restart tears the generation down and starts the
// next one from a fresh load.
func Start(ctx context.Context, dataDir string) error {
	if umaskStr := os.Getenv("UMASK"); umaskStr != "" {
		umask, err := strconv.ParseInt(umaskStr, 8, 32)
		if err != nil {
			return fmt.Errorf("invalid UMASK value: %s", umaskStr)
		}
		SetUmask(int(umask))
	}

	// chi's method table is process state: register the WebDAV verbs once,
	// before any generation builds its routers.
	webdav.RegisterMethods()

	// The rotating log file outlives generations: every component logger of
	// every generation shares this one rotator.
	logFile, err := logger.OpenRotatingFile(dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()

	reload := func() (*config.Config, error) { return config.Load(dataDir, os.LookupEnv) }
	cfg, err := reload()
	if err != nil {
		return fmt.Errorf("configuration error: %w", err)
	}
	restartCh := make(chan struct{}, 1)
	for {
		next, restart, runErr := runOnce(ctx, cfg, reload, logFile, restartCh)
		if !restart {
			return runErr
		}
		cfg = next
	}
}

// runOnce builds one generation of services from cfg under a fresh child of
// ctx and waits. After a requested restart it reports restart=true with the
// configuration for the next generation, loaded before this one was torn
// down; otherwise the process should exit with the returned error.
func runOnce(
	ctx context.Context,
	cfg *config.Config,
	reload func() (*config.Config, error),
	logFile io.Writer,
	restartCh chan struct{},
) (*config.Config, bool, error) {
	store := config.NewStore(cfg)
	logs := logger.NewFactory(cfg.LogLevel, os.Stdout, logFile)
	_log := logs.New("decypharr")

	mgr, err := manager.New(store, logs)
	if err != nil {
		return nil, false, err
	}

	svcCtx, cancelSvc := context.WithCancel(ctx)
	defer cancelSvc()

	// ascii banner
	fmt.Fprintf(os.Stdout, `
+-------------------------------------------------------+
|                                                       |
|  ╔╦╗╔═╗╔═╗╦ ╦╔═╗╦ ╦╔═╗╦═╗╦═╗                          |
|   ║║║╣ ║  └┬┘╠═╝╠═╣╠═╣╠╦╝╠╦╝ (%s)        |
|  ═╩╝╚═╝╚═╝ ┴ ╩  ╩ ╩╩ ╩╩╚═╩╚═                          |
|                                                       |
+-------------------------------------------------------+
|  Log Level: %s                                        |
+-------------------------------------------------------+
`, version.GetInfo(), cfg.LogLevel)

	// Initialize services
	mgr.SetMountManager(createMountManager(mgr, cfg, logs))
	srv := server.New(mgr, store, logs)
	srv.SetRestartFunc(func() {
		select {
		case restartCh <- struct{}{}:
		default:
		}
	})

	shutdown := func() {
		// Stop manager to cleanup all resources including mounts
		if stopErr := mgr.Stop(); stopErr != nil {
			_log.Warn().Err(stopErr).Msg("Failed to stop manager during shutdown")
		}
		// refresh GC
		runtime.GC()
	}

	serviceResult := make(chan error, 1)
	go func() {
		serviceResult <- startServices(svcCtx, mgr, cancelSvc, srv, cfg, logs)
	}()

	end, next, svcErr := awaitEnd(ctx, restartCh, serviceResult, reload, _log)
	cancelSvc()
	switch end {
	case endStopped:
		<-serviceResult
		_log.Info().Msg("Decypharr has been stopped gracefully.")
		shutdown()
		return nil, false, nil
	case endRestart:
		_log.Info().Msg("Restarting Decypharr...")
		<-serviceResult
		// The next generation builds a new manager over the same database.
		shutdown()
		_log.Info().Msg("Decypharr has been restarted.")
		return next, true, nil
	case endFailed:
		if svcErr != nil {
			_log.Error().Err(svcErr).Msg("Service stopped unexpectedly")
		}
	}
	shutdown()
	return nil, false, svcErr
}

// endKind is how a generation ends.
type endKind int

const (
	endStopped endKind = iota // ctx was cancelled
	endRestart                // a restart was requested and the next config loaded
	endFailed                 // a service stopped on its own
)

// awaitEnd waits for the generation to end. A restart request first loads
// the next generation's configuration; if that fails, the running generation
// keeps serving instead of the process exiting with nothing running.
func awaitEnd(
	ctx context.Context,
	restartCh <-chan struct{},
	serviceResult <-chan error,
	reload func() (*config.Config, error),
	log zerolog.Logger,
) (endKind, *config.Config, error) {
	for {
		select {
		case <-ctx.Done():
			return endStopped, nil, nil
		case <-restartCh:
			next, err := reload()
			if err != nil {
				log.Error().
					Err(err).
					Msg("Restart cancelled: the configuration does not load; the running services keep serving")
				continue
			}
			return endRestart, next, nil
		case err := <-serviceResult:
			return endFailed, nil, err
		}
	}
}

func createMountManager(mgr *manager.Manager, cfg *config.Config, logs *logger.Factory) manager.MountManager {
	switch cfg.Mount.Type {
	case config.MountTypeRclone:
		return rclone.NewManager(mgr, cfg, logs.New("rclone"))
	case config.MountTypeDFS:
		return dfs.NewManager(mgr, cfg, logs)
	case config.MountTypeExternalRclone:
		return external.NewManager(mgr, cfg, logs.New("external"))
	case config.MountTypeNone:
		return manager.NewStubMountManager()
	default: // unset
		return manager.NewStubMountManager()
	}
}

func startServices(
	ctx context.Context,
	manager *manager.Manager,
	cancelSvc context.CancelFunc,
	srv *server.Server,
	cfg *config.Config,
	logs *logger.Factory,
) error {
	var wg sync.WaitGroup
	// Only the first error is ever received. Sends never block, so a service
	// that fails after that (or during shutdown, when nobody receives) cannot
	// wedge wg.Wait — with NFS and SMB there are four senders.
	errChan := make(chan error, 1)
	report := func(err error) {
		select {
		case errChan <- err:
		default:
		}
	}

	_log := logs.New("decypharr")

	safeGo := func(f func() error) {
		wg.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					_log.Error().
						Interface("panic", r).
						Str("stack", string(stack)).
						Msg("Recovered from panic in goroutine")

					// Send error to channel so the main goroutine is aware
					report(fmt.Errorf("panic: %v", r))
				}
			}()

			if err := f(); err != nil {
				report(err)
			}
		})
	}

	// NFS and SMB export the same catalog through one cache: a second cache
	// over the same directory would delete the first one's files. Build it
	// before anything starts so a bad cache directory fails immediately.
	var export *share.Export
	if cfg.NFS.Enabled || cfg.SMB.Enabled {
		var err error
		if export, err = share.NewExport(ctx, manager, cfg.ShareCache, logs.New("share")); err != nil {
			return err
		}
		defer func() {
			if closeErr := export.Close(); closeErr != nil {
				_log.Error().Err(closeErr).Msg("Failed to close share export")
			}
		}()
	}

	safeGo(func() error {
		return srv.Start(ctx)
	})

	// Start manager (which handles mounts, processing, etc.)
	safeGo(func() error {
		return manager.Start(ctx)
	})

	if cfg.NFS.Enabled {
		nfs := share.NewNFS(manager, export, cfg.NFS, cfg.Dir(), logs.New("nfs"))
		safeGo(func() error {
			return nfs.Start(ctx)
		})
	}

	if cfg.SMB.Enabled {
		smb := share.NewSMB(manager, export, cfg.SMB, logs.New("smb"))
		safeGo(func() error {
			return smb.Start(ctx)
		})
	}

	select {
	case <-ctx.Done():
		wg.Wait()
		_log.Debug().Msg("Services context cancelled")
		return nil
	case err := <-errChan:
		cancelSvc()
		wg.Wait()
		return err
	}
}
