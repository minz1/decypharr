package stats

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

func TestCollectAccessState(t *testing.T) {
	t.Parallel()
	state := func(nfs, smb string) AccessState {
		return func() (string, string) { return nfs, smb }
	}
	tests := []struct {
		name    string
		nfs     bool
		smb     bool
		access  AccessState
		wantNFS string
		wantSMB string
	}{
		{"disabled", false, false, state("stopped", "stopped"), "disabled", "disabled"},
		{"running", true, true, state("running", "running"), "running", "running"},
		{"failed", true, true, state("failed", "failed"), "failed", "failed"},
		{"stopped", true, true, state("stopped", "stopped"), "stopped", "stopped"},
		{"mixed", true, false, state("failed", "running"), "failed", "disabled"},
		{"no source omits state", true, true, nil, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{}
			cfg.NFS.Enabled, cfg.SMB.Enabled = tt.nfs, tt.smb
			got := collectAccess(cfg, tt.access)
			if got.NFS.State != tt.wantNFS || got.SMB.State != tt.wantSMB {
				t.Errorf("states = (%q, %q), want (%q, %q)", got.NFS.State, got.SMB.State, tt.wantNFS, tt.wantSMB)
			}
		})
	}
}
