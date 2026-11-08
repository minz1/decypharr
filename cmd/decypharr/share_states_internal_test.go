package decypharr

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/stats"
)

func TestShareStatesReadGroupState(t *testing.T) {
	t.Parallel()
	root := task.NewRoot(t.Context(), "root", zerolog.Nop())
	t.Cleanup(root.Close)
	running := root.Child("running")

	// A failing child fails its parent, so each settled group has its own root.
	stopped := task.NewRoot(t.Context(), "stopped", zerolog.Nop())
	stopped.Close()
	<-stopped.Done()
	failed := task.NewRoot(t.Context(), "failed", zerolog.Nop())
	if err := failed.GoCritical("serve", func(context.Context) error { return errors.New("bind failed") }); err != nil {
		t.Fatal(err)
	}
	<-failed.Done()

	tests := []struct {
		name string
		nfs  *task.Group
		smb  *task.Group
		want [2]string
	}{
		{"never started", nil, nil, [2]string{stats.ShareStopped, stats.ShareStopped}},
		{"running and failed", running, failed, [2]string{stats.ShareRunning, stats.ShareFailed}},
		{"stopped", stopped, stopped, [2]string{stats.ShareStopped, stats.ShareStopped}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := &shareStates{}
			if tt.nfs != nil {
				s.setNFS(tt.nfs)
			}
			if tt.smb != nil {
				s.setSMB(tt.smb)
			}
			nfs, smb := s.accessState()()
			if nfs != tt.want[0] || smb != tt.want[1] {
				t.Errorf("states = (%q, %q), want %q", nfs, smb, tt.want)
			}
		})
	}
}
