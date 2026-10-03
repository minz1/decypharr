package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

func load(t *testing.T, dir string, vars map[string]string) *config.Config {
	t.Helper()
	cfg, err := config.Load(dir, config.MapEnv(vars))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestSessionSecretPersistsAcrossLoads(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(directory, "config.json"),
		[]byte(`{"strm":{"secret":"existing-stream-key"}}`),
		0644,
	); err != nil {
		t.Fatal(err)
	}
	first := load(t, directory, nil).SecretKey()
	if len(first) < 32 {
		t.Fatalf("session key length = %d, want at least 32", len(first))
	}
	info, err := os.Stat(filepath.Join(directory, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("config permissions = %o, want 600", info.Mode().Perm())
	}
	if load(t, directory, nil).SecretKey() != first {
		t.Fatal("session key changed after loading the same installation")
	}
	if load(t, t.TempDir(), nil).SecretKey() == first {
		t.Fatal("independent installations share a session key")
	}
}

func TestSessionSecretEnvironmentOverride(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	cfg := load(t, directory, map[string]string{"DECYPHARR_SECRET_KEY": "explicit-session-key"})
	if got := cfg.SecretKey(); got != "explicit-session-key" {
		t.Fatalf("SecretKey() = %q, want the environment override", got)
	}
	persisted := cfg.SessionSecret
	if persisted == "" || persisted == "explicit-session-key" {
		t.Fatalf("SessionSecret = %q, want a generated key", persisted)
	}
	cfg = load(t, directory, map[string]string{"DECYPHARR_SECRET_KEY": ""})
	if got := cfg.SecretKey(); got != persisted {
		t.Fatal("removing the override did not restore the persisted key")
	}
}
