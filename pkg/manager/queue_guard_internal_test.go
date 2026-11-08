package manager

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/repair"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestQueueProviderIsTheEntrysOwnOrNothing(t *testing.T) {
	t.Parallel()
	m, _ := newDroppedTestManager(t, nil)
	m.repair = repair.New(testGroup(t), repair.Dependencies{Storage: m.storage})
	if err := m.storage.AddOrUpdate(&storage.Entry{
		InfoHash: "abcdef0123", Name: "Movie", Protocol: config.ProtocolTorrent, ActiveProvider: "realdebrid",
	}); err != nil {
		t.Fatal(err)
	}

	if got := m.queueProvider("ABCDEF0123"); got != "realdebrid" {
		t.Fatalf("provider of an uppercased torrent ID = %q, want realdebrid", got)
	}
	if got := m.queueProvider("not-ours"); got != "" {
		t.Fatalf("provider of a download decypharr does not own = %q, want none so the protocol is used", got)
	}
}
