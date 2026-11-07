package manager

import (
	"context"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// refsOf is the registration count of the stream with id, or 0 when it is not registered.
func refsOf(m *Manager, id string) int {
	refs := 0
	m.activeStreams.Compute(id, func(old *ActiveStream, loaded bool) (*ActiveStream, xsync.ComputeOp) {
		if loaded {
			refs = old.refs
		}
		return old, xsync.CancelOp
	})
	return refs
}

// A stream whose parent context ends without a Close (a dropped client, a stopping server) leaves the
// active-streams view, and only once: the consumer's later Close must not release the registration again.
func TestCancelledParentUnregistersTheStream(t *testing.T) {
	t.Parallel()
	m := &Manager{activeStreams: xsync.NewMap[string, *ActiveStream]()}
	entry := &storage.Entry{
		Name: "release", InfoHash: "hash", Protocol: config.ProtocolTorrent,
		Files: map[string]*storage.File{"a.mkv": {Name: "a.mkv", ID: "1", Size: 1024}},
	}
	const id = "release:a.mkv"
	parent, cancel := context.WithCancel(t.Context())

	reader, err := m.OpenStream(parent, entry, "a.mkv", 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	// A second reader of the same file shares the registration.
	other, err := m.OpenStream(t.Context(), entry, "a.mkv", 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	if got := refsOf(m, id); got != 2 {
		t.Fatalf("registrations = %d, want 2", got)
	}

	cancel()
	eventually(t, "the cancelled reader to unregister", func() bool { return refsOf(m, id) == 1 })
	if err = reader.Close(); err != nil {
		t.Fatal(err)
	}
	if got := refsOf(m, id); got != 1 {
		t.Fatalf("registrations after the Close = %d, want the other reader's 1", got)
	}
	if err = other.Close(); err != nil {
		t.Fatal(err)
	}
	if got := m.GetActiveStreamsCount(); got != 0 {
		t.Fatalf("active streams = %d, want 0", got)
	}
}
