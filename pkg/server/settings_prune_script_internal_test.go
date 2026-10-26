package server

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"testing"
)

func TestSettingsPagePruneScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required to test the settings page scripts (the same node that runs scripts/minify-js.js)")
	}
	for _, dir := range []string{"assets/js", "assets/build/js"} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			if _, statErr := os.Stat(dir); errors.Is(statErr, fs.ErrNotExist) {
				t.Skip("UI not built; the nix check runs the minified scripts")
			}
			cmd := exec.CommandContext(t.Context(), node, "testdata/prune_harness.js", dir, "templates/config.html")
			out, runErr := cmd.CombinedOutput()
			if runErr != nil {
				t.Fatalf("%v\n%s", runErr, out)
			}
		})
	}
}
