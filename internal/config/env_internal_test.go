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
