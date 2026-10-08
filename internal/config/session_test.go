package config_test

import (
	"bytes"
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

// A debrid proxy that cannot be used is a field error when loading, not a
// silent direct connection, and it does not count against setup.
func TestLoadRejectsInvalidDebridProxy(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	body := `{"debrids":[{"name":"rd","provider":"realdebrid","api_key":"k","proxy":"http://[::1"}]}`
	if err := os.WriteFile(filepath.Join(directory, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(directory, config.MapEnv(nil))
	if err == nil || !strings.Contains(err.Error(), "debrids[0].proxy") {
		t.Fatalf("Load = %v, want a debrids[0].proxy error", err)
	}
	cfg := config.New(directory)
	cfg.Debrids = []config.Debrid{{Name: "rd", APIKey: "k", Proxy: "http://[::1"}}
	cfg.DownloadFolder = directory
	if msg := cfg.SetupError(); msg != "" {
		t.Fatalf("SetupError = %q: a bad proxy must not send the UI to setup", msg)
	}
}

// Store.Update never writes a configuration that Load would refuse, whatever
// path edits it.
func TestStoreUpdateRefusesUnloadableConfig(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	store := config.NewStore(load(t, directory, nil))
	before, err := os.ReadFile(filepath.Join(directory, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, updateErr := store.Update(func(next *config.Config) error {
		next.SharedFileMode = "rw-rw----"
		return nil
	})
	if updateErr == nil || !strings.Contains(updateErr.Error(), "shared_file_mode") {
		t.Fatalf("Update = %v, want a shared_file_mode error", updateErr)
	}
	after, err := os.ReadFile(filepath.Join(directory, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("an unloadable configuration was saved")
	}
	if _, loadErr := config.Load(directory, config.MapEnv(nil)); loadErr != nil {
		t.Fatalf("Load after the refused update: %v", loadErr)
	}
}
