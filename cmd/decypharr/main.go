package decypharr

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/task"
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
// object graph under its own task root; a requested restart drains the
// generation, re-reads the configuration and starts the next one. A failed
// generation ends the process with its error, so the service manager restarts
// it.
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

	return supervisor{
		load: func() (*config.Config, error) { return config.Load(dataDir, os.LookupEnv) },
		logs: func(cfg *config.Config) *logger.Factory {
			return logger.NewFactory(cfg.LogLevel, os.Stdout, logFile)
		},
		build:         buildServices,
		drainDeadline: generationDrainDeadline,
	}.run(ctx)
}

// buildServices starts one generation's services as children of gen.root.
// Children stop newest first, so they are created in dependency order:
//
//	manager → http → share{export, nfs, smb}
//
// The share servers read through the manager, so they stop first, then the
// server, and the manager last. The mount belongs to the manager: it starts it
// in Start and stops it in Stop, after the downloads are saved and before
// storage closes. Moving it into its own group, with a bounded unmount, is
// the P1-mount track.
func buildServices(gen *generation, cfg *config.Config, logs *logger.Factory) (func(), error) {
	root := gen.root
	log := logs.New("decypharr")
	store := config.NewStore(cfg)
	cleanup := runtime.GC // refresh GC once the generation is gone

	mgr, err := manager.New(store, logs)
	if err != nil {
		return nil, err
	}

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

	installMount(mgr, cfg, logs)
	srv := server.New(mgr, store, logs)
	srv.SetRestartFunc(gen.RequestRestart)
	srv.SetTaskSnapshot(root.Snapshot)
	srv.SetStopContext(gen.stopContext)
	gen.WaitReady(mgr.IsReady())
	gen.OnRestartRejected(func(cause error) { store.MarkRestartFailed(cause) })

	if err = root.Child("manager").GoCritical("run", func(ctx context.Context) error {
		return runManager(ctx, mgr, log)
	}); err != nil {
		return cleanup, err
	}
	if err = srv.Start(root.Child("http")); err != nil {
		return cleanup, err
	}
	return cleanup, startShares(root.Child("share"), mgr, cfg, logs)
}

// runManager runs the manager for the life of its group. The manager is
// stopped inside this task, so it happens on every path (a failed Start
// included), after the services created later have stopped, and under the
// generation's drain deadline.
//
// Known gap (P1-mount): Stop also unmounts, and the mount managers bound that
// with fixed timeouts of their own (about 10s graceful plus 10s forced), not
// with this deadline. A stuck unmount can therefore outlast the drain budget;
// the process then exits on the stop timeout.
func runManager(ctx context.Context, mgr *manager.Manager, log zerolog.Logger) error {
	defer func() {
		if err := mgr.Stop(); err != nil {
			log.Warn().Err(err).Msg("Failed to stop manager during shutdown")
		}
	}()
	if err := mgr.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

// startShares starts NFS and SMB, when enabled, over one shared export. The
// export is created first so it stops last: NFS and SMB export the same
// catalog through one cache, and a second cache over the same directory would
// delete the first one's files.
func startShares(g *task.Group, mgr *manager.Manager, cfg *config.Config, logs *logger.Factory) error {
	if !cfg.NFS.Enabled && !cfg.SMB.Enabled {
		return nil
	}
	export, err := share.NewExport(g.Child("export"), mgr, cfg.ShareCache, logs.New("share"))
	if err != nil {
		return err
	}
	if cfg.NFS.Enabled {
		nfs := share.NewNFS(mgr, export, cfg.NFS, cfg.Dir(), logs.New("nfs"))
		if err = g.Child("nfs").GoCritical("serve", nfs.Start); err != nil {
			return err
		}
	}
	if cfg.SMB.Enabled {
		smb := share.NewSMB(mgr, export, cfg.SMB, logs.New("smb"))
		if err = g.Child("smb").GoCritical("serve", smb.Start); err != nil {
			return err
		}
	}
	return nil
}

// installMount gives the manager the mount manager the configuration asks
// for. It is passed as is: the HTTP API finds the DFS cache operations by type
// assertion on it, so it must not be wrapped.
func installMount(mgr *manager.Manager, cfg *config.Config, logs *logger.Factory) {
	mgr.SetMountManager(createMountManager(mgr, cfg, logs))
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
