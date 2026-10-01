package rclone

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/rclone"
	"github.com/sirrobot01/decypharr/pkg/manager"
)

const (
	FSName     = "decypharr:"
	ConfigName = "decypharr"

	// rcloneGracefulStop is how long Stop lets rcd exit on SIGINT before
	// killing it; rcloneReapTimeout bounds the wait after the kill.
	rcloneGracefulStop = 2 * time.Second
	rcloneReapTimeout  = 5 * time.Second
	// serverReadyTimeout bounds the wait for rcd before mounting.
	serverReadyTimeout = 30 * time.Second

	// rclone.log rotation.
	logMaxSizeMB  = 10
	logMaxAgeDays = 15
	logMaxBackups = 5

	// mountRetries is how many times a failed RC mount is retried.
	mountRetries = 3
)

// Manager handles the rclone RC server and provides mount operations.
type Manager struct {
	cmd           *exec.Cmd
	configDir     string
	logger        zerolog.Logger
	ctx           context.Context
	cancel        context.CancelFunc
	serverReady   chan struct{}
	serverStarted atomic.Bool
	info          atomic.Pointer[MountInfo]
	manager       *manager.Manager
	webdavURL     string
	// exited is closed once the rcd process has been reaped. Its waiter
	// goroutine is the only cmd.Wait caller; a second concurrent Wait races
	// on the Cmd's state.
	exited     chan struct{}
	recovering atomic.Bool

	client *rclone.Client
}

type MountInfo struct {
	LocalPath  string `json:"local_path"`
	WebDAVURL  string `json:"webdav_url"`
	Mounted    bool   `json:"mounted"`
	MountedAt  string `json:"mounted_at,omitempty"`
	ConfigName string `json:"config_name"`
	Error      string `json:"error,omitempty"`
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
func NewManager(mgr *manager.Manager) manager.MountManager {
	mainCfg := config.Get()
	cfg := mainCfg.Mount
	configDir := filepath.Join(config.GetMainPath(), "rclone")
	_logger := logger.New("rclone")

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

	ctx, cancel := context.WithCancel(context.Background())
	rcServer := "http://" + net.JoinHostPort("localhost", cfg.Rclone.Port)
	rcloneClient := rclone.NewClient(rcServer, "", "", _logger)

	m := &Manager{
		configDir:   configDir,
		logger:      _logger,
		ctx:         ctx,
		cancel:      cancel,
		client:      rcloneClient,
		serverReady: make(chan struct{}),
		webdavURL:   webdavURL,
		manager:     mgr,
	}
	return m
}

// Start starts the rclone RC server.
func (m *Manager) Start(ctx context.Context) error {
	cfg := config.Get().Mount
	if m.serverStarted.Load() {
		return nil
	}
	// Use lumberjack for log rotation instead of rclone's --log-file
	rotatingLog := &lumberjack.Logger{
		Filename:   filepath.Join(logger.GetLogPath(), "rclone.log"),
		MaxSize:    logMaxSizeMB,
		MaxAge:     logMaxAgeDays,
		MaxBackups: logMaxBackups,
		Compress:   true,
	}

	args := []string{
		"rcd",
		"--rc-addr", ":" + cfg.Rclone.Port,
		"--rc-no-auth", // We'll handle auth at the application level
		"--config", filepath.Join(config.GetMainPath(), "rclone", "rclone.conf"),
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
	m.cmd = exec.CommandContext(ctx, "rclone", args...)

	// Route rclone output through lumberjack for rotation
	m.cmd.Stdout = rotatingLog
	m.cmd.Stderr = rotatingLog

	if err := m.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start rclone: %w", err)
	}
	m.serverStarted.Store(true)
	m.exited = make(chan struct{})
	go m.reap()

	// Wait for server to be ready in a goroutine
	go func() {
		defer func() {
			if r := recover(); r != nil {
				m.logger.Error().Interface("panic", r).Msg("Panic in rclone RC server monitor")
			}
		}()

		m.waitForServer()
		close(m.serverReady)

		// Start mounting here now

		if err := m.waitForReady(serverReadyTimeout); err != nil {
			m.logger.Error().Err(err).Msg("Client RC server did not become ready in time")
			return
		}

		// Start mount
		if err := m.startMount(m.ctx); err != nil {
			m.logger.Error().Err(err).Msgf("Failed to mount rclone filesystem")
		} else {
			m.logger.Info().Msgf("Successfully mounted rclone filesystem")
		}
	}()
	return nil
}

// reap waits for the rcd process and records its exit.
func (m *Manager) reap() {
	defer close(m.exited)
	err := m.cmd.Wait()
	switch {
	case err == nil:
		m.logger.Info().Msg("Client RC server exited normally")
	case errors.Is(err, context.Canceled):
		m.logger.Info().Msg("Client RC server terminated: context canceled")
	case WasHardTerminated(err): // SIGKILL on *nix; non-zero exit on Windows
		m.logger.Info().Msg("Client RC server hard-terminated")
	default:
		m.logger.Warn().Err(err).Msg("Client RC server exited")
	}
}

// Stop stops the rclone RC server and unmounts all mounts.
func (m *Manager) Stop() error {
	if !m.serverStarted.Load() {
		return nil
	}

	m.logger.Info().Msg("Stopping rclone RC server")
	// Unmount while m.ctx is live: the force-unmount fallback runs its
	// commands under it, and a canceled context kills them before they start.
	m.stopMount()
	m.cancel()

	if m.cmd != nil && m.cmd.Process != nil {
		// Try graceful shutdown first, then kill.
		if err := m.cmd.Process.Signal(os.Interrupt); err != nil {
			_ = m.cmd.Process.Kill()
		}
		select {
		case <-m.exited:
		case <-time.After(rcloneGracefulStop):
			if err := m.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return err
			}
			select {
			case <-m.exited:
				m.logger.Info().Msg("Client process cleanup completed")
			case <-time.After(rcloneReapTimeout):
				m.logger.Error().Msg("Timed out waiting for rclone to exit")
			}
		}
	}

	m.serverStarted.Store(false)
	m.logger.Info().Msg("Client RC server stopped")
	return nil
}

func (m *Manager) getMountInfo() *MountInfo {
	return m.info.Load()
}

func (m *Manager) IsMounted() bool {
	info := m.getMountInfo()
	return info != nil && info.Mounted
}

// Start creates the mount using rclone RC.
func (m *Manager) startMount(ctx context.Context) error {
	// Check if already mounted
	if m.IsMounted() {
		m.logger.Info().Msg("Mount is already mounted")
		return nil
	}

	// Try to ping rcd
	if err := m.client.Ping(ctx); err != nil {
		return fmt.Errorf("rclone RC server is not reachable: %w", err)
	}

	if err := m.mountWithRetry(ctx, mountRetries); err != nil {
		m.logger.Error().Err(err).Msg("Mount operation failed")
		return err
	}
	go m.MonitorMounts(ctx)
	return nil
}

func (m *Manager) stopMount() {
	if !m.IsMounted() {
		m.logger.Info().Msgf("Mount is not mounted, skipping unmount")
		return
	}

	m.logger.Info().Msg("Unmounting via RC")

	m.unmount(m.ctx)
	m.logger.Info().Msgf("Successfully unmounted %s", m.getMountInfo().LocalPath)
}

// IsReady returns true if the RC server is ready.
func (m *Manager) IsReady() bool {
	select {
	case <-m.serverReady:
		return true
	default:
		return false
	}
}

// Refresh refreshes directories in the VFS cache.
func (m *Manager) Refresh(dirs []string) error {
	mountInfo := m.getMountInfo()
	if mountInfo == nil || !mountInfo.Mounted {
		return fmt.Errorf("mount is not mounted")
	}

	if err := m.client.Refresh(context.Background(), dirs, FSName); err != nil {
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

// waitForServer waits for the RC server to become available.
func (m *Manager) waitForServer() {
	maxAttempts := 30
	for range maxAttempts {
		if m.ctx.Err() != nil {
			return
		}

		if err := m.client.Ping(m.ctx); err == nil {
			return
		}

		time.Sleep(time.Second)
	}

	m.logger.Error().Msg("Client RC server not responding - mount operations will be disabled")
}

// waitForReady waits for the RC server to be ready.
func (m *Manager) waitForReady(timeout time.Duration) error {
	select {
	case <-m.serverReady:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timeout waiting for rclone RC server to be ready")
	case <-m.ctx.Done():
		return m.ctx.Err()
	}
}
