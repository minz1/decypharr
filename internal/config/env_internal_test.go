package config

import (
	"os"
	"path/filepath"
	"testing"
)

// loadFresh performs a first-run Load in an empty directory with vars as the
// only environment.
func loadFresh(t *testing.T, vars map[string]string) (*Config, string) {
	t.Helper()
	dir := t.TempDir()
	c, err := Load(dir, MapEnv(vars))
	if err != nil {
		t.Fatal(err)
	}
	return c, dir
}

func TestEnvOverridesApplyOnFirstRun(t *testing.T) {
	t.Parallel()
	c, dir := loadFresh(t, map[string]string{
		"DECYPHARR_PORT":      "9191",
		"DECYPHARR_LOG_LEVEL": "debug",
	})
	if c.Port != "9191" || c.LogLevel != "debug" {
		t.Fatalf("first run ignored env: port=%q log_level=%q", c.Port, c.LogLevel)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("config.json not written on first run: %v", err)
	}
}

func TestTokenOnlyEnvOverridesReachConfig(t *testing.T) {
	t.Parallel()
	c, _ := loadFresh(t, map[string]string{
		"DECYPHARR_USE_AUTH":        "true",
		"DECYPHARR_AUTH_TOKEN_ONLY": "true",
		"DECYPHARR_API_TOKEN":       "env-token",
	})
	auth := c.GetAuth()
	if auth == nil || !auth.TokenOnly || auth.APIToken != "env-token" {
		t.Fatalf("env auth overrides lost: %+v", auth)
	}
	if c.NeedsAuth() {
		t.Fatal("token-only install from env left registration open")
	}
	if !c.VerifyToken("env-token") {
		t.Fatal("env API token does not verify")
	}
}

func TestMaxDownloadsNixAlias(t *testing.T) {
	t.Parallel()
	c, _ := loadFresh(t, map[string]string{"DECYPHARR_MAX_DOWNLOADS": "7"})
	if got := c.MaxActiveDownloads; got != 7 {
		t.Fatalf("MaxActiveDownloads = %d, want 7 from DECYPHARR_MAX_DOWNLOADS", got)
	}

	c, _ = loadFresh(t, map[string]string{"DECYPHARR_MAX_DOWNLOADS": "0"})
	if got := c.MaxActiveDownloads; got != 5 {
		t.Fatalf("MaxActiveDownloads = %d, want default 5 for the nix unset value 0", got)
	}
}

func TestRcloneMountSettingsSurviveDefaults(t *testing.T) {
	t.Parallel()
	e := env{lookup: MapEnv(map[string]string{
		"DECYPHARR_RCLONE__RC_PORT":   "6000",
		"DECYPHARR_RCLONE__LOG_LEVEL": "DEBUG",
	})}

	c := New(t.TempDir())
	c.Mount.Type = MountTypeRclone
	c.Mount.Rclone.DirCacheTime = "1h"
	c.applyEnvOverrides(e)
	c.setDefaults()
	r := c.Mount.Rclone
	if r.Port != "6000" || r.LogLevel != "DEBUG" || r.DirCacheTime != "1h" {
		t.Fatalf("mount.rclone settings overwritten: port=%q log=%q dir_cache=%q", r.Port, r.LogLevel, r.DirCacheTime)
	}

	legacy := New(t.TempDir())
	legacy.Mount.Type = MountTypeRclone
	legacy.Rclone.Port = "5000"
	legacy.setDefaults()
	if legacy.Mount.Rclone.Port != "5000" {
		t.Fatalf("legacy rclone.port not used as fallback: %q", legacy.Mount.Rclone.Port)
	}
}

func TestIndexedEnvOverrides(t *testing.T) {
	t.Parallel()
	e := env{lookup: MapEnv(map[string]string{
		"DECYPHARR_CATEGORIES__0":                    "tv",
		"DECYPHARR_CATEGORIES__1":                    "movies",
		"DECYPHARR_CATEGORIES__3":                    "ignored after a gap",
		"DECYPHARR_DEBRIDS__0__API_KEY":              "dropped: no entry 0 exists yet",
		"DECYPHARR_DEBRIDS__1__NAME":                 "torbox",
		"DECYPHARR_DEBRIDS__1__DOWNLOAD_API_KEYS__0": "dl0",
		"DECYPHARR_DEBRIDS__1__DOWNLOAD_API_KEYS__1": "dl1",
		"DECYPHARR_ARRS__0__TOKEN":                   "no entry to attach to",
		"DECYPHARR_NFS__PORT":                        "70000", // out of range: ignored
	})}

	c := &Config{Categories: []string{"old"}, NFS: NFS{Port: 1}}
	c.applyEnvOverrides(e)

	if len(c.Categories) != 2 || c.Categories[0] != "tv" || c.Categories[1] != "movies" {
		t.Errorf("Categories = %q", c.Categories)
	}
	if len(c.Debrids) != 2 || c.Debrids[0].APIKey != "" || c.Debrids[1].Name != "torbox" ||
		len(c.Debrids[1].DownloadAPIKeys) != 2 || c.Debrids[1].DownloadAPIKeys[1] != "dl1" {
		t.Errorf("Debrids = %+v", c.Debrids)
	}
	if len(c.Arrs) != 0 {
		t.Errorf("TOKEN without NAME created an arr: %+v", c.Arrs)
	}
	if c.NFS.Port != 1 {
		t.Errorf("NFS.Port = %d, want unchanged for an out-of-range value", c.NFS.Port)
	}
}

// The healthcheck probes a running container; loading the config must not
// create or rewrite anything in its data folder.
func TestLoadReadOnlyWritesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	c, err := LoadReadOnly(missing, MapEnv(map[string]string{
		"DECYPHARR_USE_AUTH":        "true",
		"DECYPHARR_AUTH_TOKEN_ONLY": "true",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != DefaultPort {
		t.Fatalf("Port = %q, want the default", c.Port)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("read-only load created the data folder: %v", statErr)
	}

	existing := []byte(`{"port":"1234"}`)
	if writeErr := os.WriteFile(filepath.Join(dir, "config.json"), existing, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	c, err = LoadReadOnly(dir, MapEnv(map[string]string{"DECYPHARR_USE_AUTH": "true"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != "1234" {
		t.Fatalf("Port = %q, want 1234 from config.json", c.Port)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("read-only load wrote files: %v", entries)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "config.json")); string(data) != string(existing) {
		t.Fatalf("read-only load rewrote config.json: %s", data)
	}
	if saveErr := c.Save(); saveErr == nil {
		t.Fatal("Save succeeded on a read-only config")
	}
}
