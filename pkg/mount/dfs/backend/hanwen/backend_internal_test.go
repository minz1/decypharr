//go:build linux || (darwin && amd64)

package hanwen

import (
	"context"
	"errors"
	"testing"
)

type stubServer struct {
	err error
}

func (s stubServer) Unmount() error { return s.err }

// recordingDetacher records the mount points it detached.
type recordingDetacher struct {
	paths []string
}

func (r *recordingDetacher) Unmount(_ context.Context, path string) error {
	r.paths = append(r.paths, path)
	return nil
}

// The handle's graceful unmount is the server's, and Detach lazily detaches
// the mount path without touching the server.
func TestMountedUnmountAndDetach(t *testing.T) {
	t.Parallel()
	errBusy := errors.New("device busy")
	detacher := &recordingDetacher{}
	h := newMounted(stubServer{err: errBusy}, "/mnt/dfs", detacher)

	if err := h.Unmount(); !errors.Is(err, errBusy) {
		t.Fatalf("Unmount = %v, want %v", err, errBusy)
	}
	if len(detacher.paths) != 0 {
		t.Fatalf("graceful unmount detached %v", detacher.paths)
	}
	if err := h.Detach(t.Context()); err != nil {
		t.Fatalf("Detach = %v", err)
	}
	if len(detacher.paths) != 1 || detacher.paths[0] != "/mnt/dfs" {
		t.Fatalf("detached %v, want [/mnt/dfs]", detacher.paths)
	}
}
