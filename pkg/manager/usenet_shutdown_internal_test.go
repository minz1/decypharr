package manager

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet"
	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
)

// parsingNZB stages nzb-1 with usenet meta that is still parsing and returns the call of its submit child.
func parsingNZB(t *testing.T) (*Manager, entryChildCall) {
	t.Helper()
	m, call := newParseChildCall(
		t,
		func(context.Context, string, string, []byte, string) (*storage.NZB, map[string]*parser.FileGroup, error) {
			return &storage.NZB{ID: "nzb-1", Name: "Show.S01"}, parsedGroups(), nil
		},
	)
	meta := &storage.NZB{ID: "nzb-1", Name: "Show.S01", Path: call.grab.Entry.Magnet, Status: usenet.NZBStatusParsing}
	if err := m.usenet.NZBStorage().AddNZB(meta); err != nil {
		t.Fatal(err)
	}
	return m, call
}

// Shutdown cancels the child while Process runs. The usenet meta must not be failed, and the staged source stays
// for the next start's Recover.
func TestShutdownDuringNZBProcessKeepsMetaParsing(t *testing.T) {
	t.Parallel()
	m, call := parsingNZB(t)
	m.usenetTimeout = time.Minute
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if msg := m.parseChild(ctx, call); msg.ev != nil {
		t.Fatalf("parse result = %+v, want none after a shutdown", msg.ev)
	}

	header, err := m.usenet.NZBStorage().GetNZBHeader("nzb-1")
	if err != nil {
		t.Fatal(err)
	}
	if header.Status != usenet.NZBStatusParsing {
		t.Fatalf("meta status = %q after shutdown, want %q", header.Status, usenet.NZBStatusParsing)
	}
	if _, statErr := os.Stat(call.grab.Entry.Magnet); statErr != nil {
		t.Fatalf("source NZB lost by the interrupted run: %v", statErr)
	}
}
