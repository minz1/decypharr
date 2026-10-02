package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

func TestSessionSecretPersistsAcrossLoads(t *testing.T) {
	t.Setenv("DECYPHARR_SECRET_KEY", "")
	config.Reset()
	directory := t.TempDir()
	config.SetConfigPath(directory)
	t.Cleanup(config.Reset)
	if err := os.WriteFile(
		filepath.Join(directory, "config.json"),
		[]byte(`{"strm":{"secret":"existing-stream-key"}}`),
		0644,
	); err != nil {
		t.Fatal(err)
	}
	first := config.Get().SecretKey()
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
	config.Reset()
	if config.Get().SecretKey() != first {
		t.Fatal("session key changed after loading the same installation")
	}
	config.Reset()
	config.SetConfigPath(t.TempDir())
	if config.Get().SecretKey() == first {
		t.Fatal("independent installations share a session key")
	}
}

func TestSessionSecretEnvironmentOverride(t *testing.T) {
	t.Setenv("DECYPHARR_SECRET_KEY", "explicit-session-key")
	cfg := &config.Config{SessionSecret: "persisted-session-key"}
	if got := cfg.SecretKey(); got != "explicit-session-key" {
		t.Fatalf("SecretKey() = %q, want the environment override", got)
	}
	t.Setenv("DECYPHARR_SECRET_KEY", "")
	if got := cfg.SecretKey(); got != cfg.SessionSecret {
		t.Fatal("removing the override did not restore the persisted key")
	}
}
