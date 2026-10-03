//go:build linux || (darwin && amd64)

package hanwen

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
)

type stubServer struct {
	unmount func() error
}

func (s stubServer) Unmount() error { return s.unmount() }

// recordingDetacher records force unmounts and whether their context was
// already done.
type recordingDetacher struct {
	calls     int
	ctxWasDue bool
}

func (r *recordingDetacher) Unmount(ctx context.Context, _ string) error {
	r.calls++
	r.ctxWasDue = ctx.Err() != nil
	return nil
}

// A clean unmount returns at once and does not force; a failed or stuck one
// forces with a context that is still live.
func TestUnmountServer(t *testing.T) {
	t.Parallel()
	errBusy := errors.New("device busy")
	tests := map[string]struct {
		unmount   func(release <-chan struct{}) error
		wantForce bool
	}{
		"clean":  {func(<-chan struct{}) error { return nil }, false},
		"failed": {func(<-chan struct{}) error { return errBusy }, true},
		"stuck": {func(release <-chan struct{}) error {
			<-release
			return nil
		}, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				detacher := &recordingDetacher{}
				b := &Backend{
					config:    &config.FuseConfig{MountPath: t.TempDir()},
					logger:    zerolog.Nop(),
					unmounter: detacher,
				}
				release := make(chan struct{})
				defer close(release)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()

				start := time.Now()
				b.unmountServer(ctx, stubServer{unmount: func() error { return tt.unmount(release) }})
				elapsed := time.Since(start)

				if got := detacher.calls > 0; got != tt.wantForce {
					t.Errorf("forced = %v, want %v", got, tt.wantForce)
				}
				if detacher.ctxWasDue {
					t.Error("force unmount got an expired context")
				}
				if name != "stuck" && elapsed != 0 {
					t.Errorf("unmount took %v, want no added delay", elapsed)
				}
			})
		})
	}
}
