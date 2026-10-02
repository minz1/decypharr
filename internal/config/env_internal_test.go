package config

import (
	"os"
	"path/filepath"
	"testing"
)

// freshConfig points the singleton at an empty directory, so Get performs a
// first-run load.
func freshConfig(t *testing.T) string {
	t.Helper()
	Reset()
	dir := t.TempDir()
	SetConfigPath(dir)
	t.Cleanup(Reset)
	return dir
}

func TestEnvOverridesApplyOnFirstRun(t *testing.T) {
	t.Setenv("DECYPHARR_PORT", "9191")
	t.Setenv("DECYPHARR_LOG_LEVEL", "debug")
	dir := freshConfig(t)

	c := Get()
	if c.Port != "9191" || c.LogLevel != "debug" {
		t.Fatalf("first run ignored env: port=%q log_level=%q", c.Port, c.LogLevel)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("config.json not written on first run: %v", err)
	}
}

func TestTokenOnlyEnvOverridesReachConfig(t *testing.T) {
	t.Setenv("DECYPHARR_USE_AUTH", "true")
	t.Setenv("DECYPHARR_AUTH_TOKEN_ONLY", "true")
	t.Setenv("DECYPHARR_API_TOKEN", "env-token")
	freshConfig(t)

	c := Get()
	auth := c.GetAuth()
	if auth == nil || !auth.TokenOnly || auth.APIToken != "env-token" {
		t.Fatalf("env auth overrides lost: %+v", auth)
	}
	if c.NeedsAuth() {
		t.Fatal("token-only install from env left registration open")
	}
	if !VerifyToken("env-token") {
		t.Fatal("env API token does not verify")
	}
}

func TestMaxDownloadsNixAlias(t *testing.T) {
	t.Setenv("DECYPHARR_MAX_DOWNLOADS", "7")
	freshConfig(t)
	if got := Get().MaxActiveDownloads; got != 7 {
		t.Fatalf("MaxActiveDownloads = %d, want 7 from DECYPHARR_MAX_DOWNLOADS", got)
	}

	t.Setenv("DECYPHARR_MAX_DOWNLOADS", "0")
	freshConfig(t)
	if got := Get().MaxActiveDownloads; got != 5 {
		t.Fatalf("MaxActiveDownloads = %d, want default 5 for the nix unset value 0", got)
	}
}

func TestRcloneMountSettingsSurviveDefaults(t *testing.T) {
	t.Setenv("DECYPHARR_RCLONE__RC_PORT", "6000")
	t.Setenv("DECYPHARR_RCLONE__LOG_LEVEL", "DEBUG")
	freshConfig(t)

	c := &Config{Mount: Mount{Type: MountTypeRclone}}
	c.Mount.Rclone.DirCacheTime = "1h"
	c.applyEnvOverrides()
	c.setDefaults()
	r := c.Mount.Rclone
	if r.Port != "6000" || r.LogLevel != "DEBUG" || r.DirCacheTime != "1h" {
		t.Fatalf("mount.rclone settings overwritten: port=%q log=%q dir_cache=%q", r.Port, r.LogLevel, r.DirCacheTime)
	}

	legacy := &Config{Mount: Mount{Type: MountTypeRclone}, Rclone: Rclone{Port: "5000"}}
	legacy.setDefaults()
	if legacy.Mount.Rclone.Port != "5000" {
		t.Fatalf("legacy rclone.port not used as fallback: %q", legacy.Mount.Rclone.Port)
	}
}

func TestIndexedEnvOverrides(t *testing.T) {
	t.Setenv("DECYPHARR_CATEGORIES__0", "tv")
	t.Setenv("DECYPHARR_CATEGORIES__1", "movies")
	t.Setenv("DECYPHARR_CATEGORIES__3", "ignored after a gap")
	t.Setenv("DECYPHARR_DEBRIDS__0__API_KEY", "dropped: no entry 0 exists yet")
	t.Setenv("DECYPHARR_DEBRIDS__1__NAME", "torbox")
	t.Setenv("DECYPHARR_DEBRIDS__1__DOWNLOAD_API_KEYS__0", "dl0")
	t.Setenv("DECYPHARR_DEBRIDS__1__DOWNLOAD_API_KEYS__1", "dl1")
	t.Setenv("DECYPHARR_ARRS__0__TOKEN", "no entry to attach to")
	t.Setenv("DECYPHARR_NFS__PORT", "70000") // out of range: ignored

	c := &Config{Categories: []string{"old"}, NFS: NFS{Port: 1}}
	c.applyEnvOverrides()

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
