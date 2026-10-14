package storage_test

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestEntryMergeKeepsManual(t *testing.T) {
	t.Parallel()
	for _, protocol := range []config.Protocol{config.ProtocolTorrent, config.ProtocolNZB} {
		for _, tt := range []struct {
			name                     string
			existing, incoming, want bool
		}{
			{"stored manual survives re-add", true, false, true},
			{"incoming manual upgrades", false, true, true},
			{"neither", false, false, false},
		} {
			t.Run(string(protocol)+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				existing := &storage.Entry{InfoHash: "h", Protocol: protocol, Manual: tt.existing}
				incoming := &storage.Entry{InfoHash: "h", Protocol: protocol, Manual: tt.incoming}
				if got := storage.HandleExistingEntryMerge(existing, incoming).Manual; got != tt.want {
					t.Fatalf("Manual = %v, want %v", got, tt.want)
				}
			})
		}
	}
}
