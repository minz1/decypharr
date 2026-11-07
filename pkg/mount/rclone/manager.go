package rclone

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/rclone"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/task/keyed"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/mount/unmount"
)

const (
	FSName     = "decypharr:"
	ConfigName = "decypharr"

	// rcloneGracefulStop is how long Stop lets rcd exit on SIGINT before
	// killing it; rcloneReapTimeout bounds the wait after the kill.
	rcloneGracefulStop = 2 * time.Second
	rcloneReapTimeout  = 5 * time.Second

	// managerStopTimeout bounds Stop: the unmount, the signal, the kill and the reap.
	managerStopTimeout = 30 * time.Second
	// rcCallTimeout bounds one RC call made for a caller that has no context.
	rcCallTimeout = 10 * time.Second

	// rclone.log rotation.
	logMaxSizeMB  = 10
	logMaxAgeDays = 15
	logMaxBackups = 5
)

// errStarted is returned by a second Start.
var errStarted = errors.New("rclone manager already started")

// Manager handles the rclone RC server and provides mount operations. The
// rcdOwner runs the rcd process; the manager mounts when it becomes Ready.
type Manager struct {
	configDir    string
	logsDir      string
	mount        config.Mount
	mountDirMode fs.FileMode
	unmounter    *unmount.Unmounter
	// detach unmounts a path without rclone's help; the unmounter by default.
	detach func(ctx context.Context, path string) error
	logger zerolog.Logger
	// view is the mount's state; moveMount is its only writer.
	view atomic.Pointer[mountView]
	// mountRefused counts moves the mount's table refused.
	mountRefused atomic.Uint64
	manager      *manager.Manager
	webdavURL    string
	rcdLog       *lumberjack.Logger

	client *rclone.Client
	owner  *rcdOwner

	// mu guards Start against Stop. rcd, ops and sup are written once, in
	// Start; the tasks started after that read them without the lock.
	mu sync.Mutex
	// rcd runs the owner and its reapers; ops runs the mount supervisor and
	// its children. ops is the newer child, so it stops first: the supervisor
	// ends, then the owner unmounts through RC, then rcd stops.
	rcd *task.Group
	ops *task.Group
	sup *keyed.Singleton[supMsg, supState]
}

type MountInfo struct {
	LocalPath  string `json:"local_path"`
	WebDAVURL  string `json:"webdav_url"`
	Mounted    bool   `json:"mounted"`
	MountedAt  string `json:"mounted_at,omitempty"`
	ConfigName string `json:"config_name"`
	Error      string `json:"error,omitempty"`
	// State is the mount's phase, shown next to Error.
	State string `json:"state"`
}

type RCRequest struct {
	Command string         `json:"command"`
	Args    map[string]any `json:"args,omitempty"`
}

type RCResponse struct {
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// NewManager creates a new rclone RC manager. When WebDAV is disabled rclone
// has nothing to mount, so it returns a no-op manager — never a nil *Manager,
// which would become a non-nil interface that panics on first use.
func NewManager(mgr *manager.Manager, mainCfg *config.Config, _logger zerolog.Logger) manager.MountManager {
	cfg := mainCfg.Mount
	configDir := filepath.Join(mainCfg.Dir(), "rclone")

	if mainCfg.DisableWebDav {
		_logger.Info().Msg("WebDAV support is disabled by configuration, can't use rclone with WebDAV features")
		return manager.NewStubMountManager()
	}

	// Ensure config directory exists
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		_logger.Error().Err(err).Msg("Failed to create rclone config directory")
	}

	bindAddress := mainCfg.BindAddress
	if bindAddress == "" {
		bindAddress = "localhost"
	}

	baseURL := "http://" + net.JoinHostPort(bindAddress, mainCfg.Port)
	webdavURL, err := url.JoinPath(baseURL, mainCfg.URLBase, "webdav")
	if err != nil {
		_logger.Error().Err(err).Msg("Invalid WebDAV URL, rclone mount disabled")
		return manager.NewStubMountManager()
	}

	if !strings.HasSuffix(webdavURL, "/") {
		webdavURL += "/"
	}

	logsDir := logger.Dir(mainCfg.Dir())
	rcServer := "http://" + net.JoinHostPort("localhost", cfg.Rclone.Port)
	m := &Manager{
		configDir:    configDir,
		logsDir:      logsDir,
		mount:        cfg,
		mountDirMode: mainCfg.SharedDirModeValue(),
		unmounter:    unmount.New(_logger),
		logger:       _logger,
		client:       rclone.NewClient(rcServer, "", "", mgr.TLSConfig(), _logger),
		webdavURL:    webdavURL,
		manager:      mgr,
		// Lumberjack rotates rclone.log; rclone's own --log-file does not.
		rcdLog: &lumberjack.Logger{
			Filename:   filepath.Join(logsDir, "rclone.log"),
			MaxSize:    logMaxSizeMB,
			MaxAge:     logMaxAgeDays,
			MaxBackups: logMaxBackups,
			Compress:   true,
		},
	}
	m.wire()
	return m
}

// wire builds the rcd owner and connects it to the manager.
func (m *Manager) wire() {
	o := newRcdOwner(nil, m.logger)
	o.spawn = m.spawnRcd
	o.ping = m.client.Ping
	o.onReady = m.onRcdReady
	o.onExit = m.onRcdExit
	if m.detach == nil {
		m.detach = m.unmounter.Unmount
	}
	o.beforeStop = m.unmount
	o.closeLog = m.closeLog
	m.owner = o
	m.view.Store(&mountView{state: mountWaiting})
}

// Start starts the rcd owner and the monitor on children of g. It returns
// before rcd answers; IsReady says when it does.
func (m *Manager) Start(g *task.Group) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.owner.state.Load()
	if current == rcdStopping || current == rcdStopped {
		return task.ErrStopped
	}
	if current != rcdNew {
		return errStarted
	}
	m.owner.transition(rcdNew, rcdStarting)
	m.rcd = g.Child("rcd")
	m.ops = g.Child("ops")
	m.owner.group = m.rcd
	// The supervisor exists before the owner can report rcd Ready to it.
	m.sup = keyed.NewSingleton(m.ops, "supervisor", mountSupervisor{m: m}, keyed.Options[supMsg]{Logger: m.logger})

	if err := m.rcd.Go("owner", m.owner.run); err != nil {
		// The owner never ran, so nothing else ends this state.
		m.owner.transition(rcdStarting, rcdStopping)
		m.owner.transition(rcdStopping, rcdStopped)
		return fmt.Errorf("start the rcd owner: %w", err)
	}
	return nil
}

// Stop stops the mount work, unmounts through RC and stops rcd, in that order.
// Before Start it only closes the manager to a later Start.
func (m *Manager) Stop() error {
	m.mu.Lock()
	if m.owner.state.Load() == rcdNew {
		m.owner.transition(rcdNew, rcdStopped)
		m.mu.Unlock()
		return nil
	}
	rcd, ops := m.rcd, m.ops
	m.mu.Unlock()
	if rcd == nil || ops == nil {
		return nil
	}

	m.logger.Info().Msg("Stopping rclone RC server")
	ctx, cancel := task.Detached(rcd.Context(), managerStopTimeout)
	defer cancel()
	err := errors.Join(ops.Stop(ctx), rcd.Stop(ctx))
	if current := m.owner.state.Load(); err == nil && current != rcdStopped {
		err = fmt.Errorf("rcd did not stop: state %s", current)
	}
	if err != nil {
		return err
	}
	m.logger.Info().Msg("Client RC server stopped")
	return nil
}

// spawnRcd starts the rcd process. Its output goes to the rotating log.
func (m *Manager) spawnRcd() (rcdProcess, error) {
	cfg := m.mount
	args := []string{
		"rcd",
		"--rc-addr", ":" + cfg.Rclone.Port,
		"--rc-no-auth", // We'll handle auth at the application level
		"--config", filepath.Join(m.configDir, "rclone.conf"),
		// No --log-file, we capture output directly
	}

	logLevel := cfg.Rclone.LogLevel
	if logLevel != "" {
		if !slices.Contains([]string{"DEBUG", "INFO", "NOTICE", "ERROR"}, logLevel) {
			logLevel = "INFO"
		}
		args = append(args, "--log-level", logLevel)
	}

	if cfg.Rclone.CacheDir != "" {
		if err := os.MkdirAll(cfg.Rclone.CacheDir, 0o750); err == nil {
			args = append(args, "--cache-dir", cfg.Rclone.CacheDir)
		}
	}
	// The owner signals the process in order, so the group ending must not kill it.
	cmd := exec.CommandContext(context.WithoutCancel(m.owner.group.Context()), "rclone", args...)
	cmd.Stdout = m.rcdLog
	cmd.Stderr = m.rcdLog
	ownProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start rclone: %w", err)
	}
	return &execProcess{cmd: cmd}, nil
}

// closeLog closes the rotating log once the owner has ended.
func (m *Manager) closeLog() error {
	if m.rcdLog == nil {
		return nil
	}
	return m.rcdLog.Close()
}

// onRcdReady tells the supervisor that rcd answers.
func (m *Manager) onRcdReady() { m.sendSup(supRcdReady) }

// onRcdExit tells the supervisor that rcd exited, taking the mount with it.
func (m *Manager) onRcdExit() { m.sendSup(supRcdDied) }

// IsMounted reports a mount that serves: Mounted, or Degraded after a failed probe.
func (m *Manager) IsMounted() bool {
	s := m.view.Load().state
	return s == mountMounted || s == mountDegraded
}

// IsReady returns true while rcd answers.
func (m *Manager) IsReady() bool {
	return m.owner.state.Load() == rcdReady
}

// Refresh refreshes directories in the VFS cache.
func (m *Manager) Refresh(dirs []string) error {
	if !m.IsMounted() || !m.IsReady() {
		return fmt.Errorf("mount is not mounted")
	}

	ctx, cancel := context.WithTimeout(m.owner.group.Context(), rcCallTimeout)
	defer cancel()
	if err := m.client.Refresh(ctx, dirs, FSName); err != nil {
		m.logger.Error().Err(err).
			Msg("Failed to refresh directory")
		return fmt.Errorf("failed to refresh directory %s : %w", dirs, err)
	}
	return nil
}

func (m *Manager) GetLogger() zerolog.Logger {
	return m.logger
}

func (m *Manager) Type() string {
	return "rclone"
}
