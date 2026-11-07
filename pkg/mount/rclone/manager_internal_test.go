package rclone

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
)

// loadConfig loads a configuration from a temp dir holding body.
func loadConfig(t *testing.T, body string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir, config.MapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// With WebDAV disabled NewManager returned a nil *Manager, which became a
// non-nil MountManager that panicked on Start.
func TestNewManagerWithWebDAVDisabledIsUsable(t *testing.T) {
	t.Parallel()
	cfg := loadConfig(t, `{"disable_webdav": true}`)

	m := NewManager(nil, cfg, zerolog.Nop())
	if m == nil {
		t.Fatal("NewManager returned nil")
	}
	if err := m.Start(task.NewRoot(t.Context(), "test", zerolog.Nop())); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// RecoverMount used to call Start, a no-op once the RC server runs, so a
// failed health check left the mount down for good.
func TestRecoverMountRemounts(t *testing.T) {
	t.Parallel()
	r := newManagerRig(t)
	r.m.info.Store(&MountInfo{LocalPath: "/mnt/x", Mounted: false, Error: "Health check failed"})

	if err := r.m.RecoverMount(t.Context()); err != nil {
		t.Fatalf("RecoverMount: %v", err)
	}
	if got := r.count("rc:mount/mount"); got != 1 {
		t.Fatalf("recovery asked rclone to mount %d times, want 1", got)
	}
	if !r.m.IsMounted() {
		t.Fatal("mount not marked mounted after recovery")
	}
}
