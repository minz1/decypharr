package version

import (
	"runtime/debug"
	"testing"
)

func TestResolve(t *testing.T) {
	t.Parallel()
	vcs := &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef0123"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	for _, tc := range []struct {
		name      string
		stamp     string
		buildInfo *debug.BuildInfo
		want      Info
	}{
		{"release stamp", "2.4.0 stable\n", vcs, Info{Version: "2.4.0", Channel: "stable"}},
		{"stamp without channel", "2.4.0", nil, Info{Version: "2.4.0", Channel: "dev"}},
		{"vcs build", "", vcs, Info{Version: "dev+0123456789ab-dirty", Channel: "dev"}},
		{"go install", "", &debug.BuildInfo{Main: debug.Module{Version: "v2.4.1"}}, Info{Version: "2.4.1", Channel: "dev"}},
		{"no build info", "  \n", nil, Info{Version: "dev", Channel: "dev"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolve(tc.stamp, tc.buildInfo); got != tc.want {
				t.Fatalf("resolve = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The committed release.txt must stay empty: only release builds stamp it.
func TestCommittedStampIsEmpty(t *testing.T) {
	t.Parallel()
	if release != "" {
		t.Fatalf("release.txt = %q; restore the empty file after a release build", release)
	}
}
