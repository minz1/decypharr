package rclone

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/rclone"
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
	if err := m.Start(context.Background()); err != nil {
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
	cfg := loadConfig(t, `{"mount": {"mount_path": "`+filepath.ToSlash(t.TempDir())+`"}}`)

	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, strings.TrimPrefix(r.URL.Path, "/"))
		mu.Unlock()
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := &Manager{
		logger: zerolog.Nop(),
		mount:  cfg.Mount,
		ctx:    ctx,
		cancel: cancel,
		client: rclone.NewClient(srv.URL, "", "", zerolog.Nop()),
	}
	m.info.Store(&MountInfo{LocalPath: "/mnt/x", Mounted: false, Error: "Health check failed"})

	if err := m.RecoverMount(ctx); err != nil {
		t.Fatalf("RecoverMount: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	mounted := false
	for _, c := range calls {
		if c == "mount/mount" {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("recovery never asked rclone to mount; RC calls: %v", calls)
	}
	if !m.IsMounted() {
		t.Fatal("mount not marked mounted after recovery")
	}
}
