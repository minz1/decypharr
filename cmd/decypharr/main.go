package decypharr

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"

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
//	manager → http → mount{run, stopping} → share{export, nfs, smb}
//
// The share servers read through the manager, so they stop first; then the
// mount, whose "stopping" child closes the mount gate and arms the mount's
// 8s slice before the mount's own stop begins; then the server, and the
// manager last. A stopping mount therefore never fails a grab: the manager's
// mount-dependent steps end resumable on the gate. Before the mount stops,
// the server has not stopped, so the HTTP API is still served.
func buildServices(gen *generation, cfg *config.Config, logs *logger.Factory) (func(), error) {
	root := gen.root
	store := config.NewStore(cfg)
	slice := newMountSlice(gen)
	cleanup := func() {
		slice.release()
		runtime.GC() // refresh GC once the generation is gone
	}

	mgr, err := manager.New(root.Child("manager"), store, logs)
	if err != nil {
		return cleanup, err
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
	gate := manager.NewMountGate(logs.New("mount"))
	mgr.SetMountGate(gate)
	shares := &shareStates{}
	srv := server.New(mgr.Context(), mgr, store, logs, shares.accessState())
	srv.SetRestartFunc(gen.RequestRestart)
	srv.SetTaskSnapshot(root.Snapshot)
	srv.SetStopContext(gen.stopContext)
	gen.WaitReady(mgr.IsReady())
	gen.OnRestartRejected(func(cause error) { store.MarkRestartFailed(cause) })

	// A start step that fails fails the manager's group, and with it the
	// generation. The mount group starts the mount once the manager is ready;
	// a mount that fails to start is logged and the generation goes on.
	if err = mgr.Start(); err != nil {
		return cleanup, err
	}
	if err = srv.Start(root.Child("http")); err != nil {
		return cleanup, err
	}
	mm, mountLog := mgr.MountManager(), logs.New("mount")
	if err = startMountGroup(root, mm, mgr.IsReady(), gate, slice, srv.Stopped(), mountLog); err != nil {
		return cleanup, err
	}
	return cleanup, startShares(root.Child("share"), shares, mgr, cfg, logs)
}

// startShares starts NFS and SMB, when enabled, over one shared export. The
// export is created first so it stops last: NFS and SMB export the same
// catalog through one cache, and a second cache over the same directory would
// delete the first one's files. Each started server's group is recorded in
// states for the stats collector.
func startShares(
	g *task.Group, states *shareStates, mgr *manager.Manager, cfg *config.Config, logs *logger.Factory,
) error {
	if !cfg.NFS.Enabled && !cfg.SMB.Enabled {
		return nil
	}
	export, err := share.NewExport(g.Child("export"), mgr, cfg.ShareCache, logs.New("share"))
	if err != nil {
		return err
	}
	if cfg.NFS.Enabled {
		nfs := share.NewNFS(mgr, export, cfg.NFS, cfg.Dir(), logs.New("nfs"))
		group := g.Child("nfs")
		states.setNFS(group)
		if err = group.GoCritical("serve", nfs.Start); err != nil {
			return err
		}
	}
	if cfg.SMB.Enabled {
		smb := share.NewSMB(mgr, export, cfg.SMB, logs.New("smb"))
		group := g.Child("smb")
		states.setSMB(group)
		if err = group.GoCritical("serve", smb.Start); err != nil {
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
