package config_test

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

func TestDownloadActionForUsesTheCategoryOverride(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		DefaultDownloadAction:   config.DownloadActionSymlink,
		CategoryDownloadActions: map[string]config.DownloadAction{"bindery": config.DownloadActionDownload},
	}
	for category, want := range map[string]config.DownloadAction{
		"bindery": config.DownloadActionDownload,
		"sonarr":  config.DownloadActionSymlink,
		"":        config.DownloadActionSymlink,
	} {
		if got := cfg.DownloadActionFor(category); got != want {
			t.Errorf("DownloadActionFor(%q) = %q, want %q", category, got, want)
		}
	}
}

func TestCheckLoadableRejectsAnUnknownCategoryDownloadAction(t *testing.T) {
	t.Parallel()
	bad := &config.Config{CategoryDownloadActions: map[string]config.DownloadAction{"bindery": "copy"}}
	if err := bad.CheckLoadable(); err == nil {
		t.Fatal("an unknown category download action was accepted")
	}
	for _, action := range []config.DownloadAction{
		config.DownloadActionSymlink, config.DownloadActionDownload, config.DownloadActionNone,
	} {
		good := &config.Config{CategoryDownloadActions: map[string]config.DownloadAction{"bindery": action}}
		if err := good.CheckLoadable(); err != nil {
			t.Errorf("action %q refused: %v", action, err)
		}
	}
}

func TestCategoryDownloadActionsApplyWithoutRestart(t *testing.T) {
	t.Parallel()
	before := &config.Config{}
	after := &config.Config{
		CategoryDownloadActions: map[string]config.DownloadAction{"bindery": config.DownloadActionDownload},
	}
	if before.RequiresRestart(after) {
		t.Fatal("a category download action change required a restart")
	}
}
