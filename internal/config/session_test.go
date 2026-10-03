package config_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	persisted := cfg.SessionSecret.Reveal()
	if persisted == "" || persisted == "explicit-session-key" {
		t.Fatal("SessionSecret is not a generated key")
	}
	cfg = load(t, directory, map[string]string{"DECYPHARR_SECRET_KEY": ""})
	if got := cfg.SecretKey(); got != persisted {
		t.Fatal("removing the override did not restore the persisted key")
	}
}

// Earlier versions kept the session secret in config.json. It keeps working
// and moves to secrets.json (0600) on the first save.
func TestSessionSecretMigratesToSecretsFile(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	legacy := `{"session_secret":"legacy-session-key","strm":{"secret":"strm-key"}}`
	if err := os.WriteFile(filepath.Join(directory, "config.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := load(t, directory, nil)
	if got := cfg.SecretKey(); got != "legacy-session-key" {
		t.Fatalf("SecretKey() = %q, want the legacy key", got)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "legacy-session-key") || strings.Contains(string(data), "session_secret") {
		t.Fatalf("config.json still holds the session secret: %s", data)
	}
	secrets, err := os.ReadFile(cfg.SecretsFile())
	if err != nil || !strings.Contains(string(secrets), "legacy-session-key") {
		t.Fatalf("secrets.json = %q, %v", secrets, err)
	}
	if info, statErr := os.Stat(cfg.SecretsFile()); statErr != nil ||
		(runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("secrets.json mode: %v, %v", info, statErr)
	}
	if got := load(t, directory, nil).SecretKey(); got != "legacy-session-key" {
		t.Fatalf("after migration SecretKey() = %q", got)
	}
}

// The session secret never reaches a JSON encoding or a formatted string.
func TestSessionSecretIsNotSerialized(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{SessionSecret: config.NewSecret("hidden-key")}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hidden-key") {
		t.Fatalf("config JSON leaked the secret: %s", data)
	}
	for _, text := range []string{fmt.Sprint(cfg.SessionSecret), fmt.Sprintf("%#v", cfg.SessionSecret)} {
		if strings.Contains(text, "hidden-key") {
			t.Fatalf("formatting leaked the secret: %s", text)
		}
	}
	clone, err := cfg.Clone()
	if err != nil || clone.SessionSecret.Reveal() != "hidden-key" {
		t.Fatalf("Clone lost the secret: %v", err)
	}
}
