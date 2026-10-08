package parser

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/types"
)

// bodySource serves fixed article bodies by message id.
type bodySource struct {
	bodies map[string][]byte
}

func (s bodySource) Header(context.Context, string, int) (*nntp.YencMetadata, error) {
	return nil, io.EOF
}

func (s bodySource) Body(_ context.Context, id string) ([]byte, error) {
	return s.bodies[id], nil
}

func (s bodySource) Stat(context.Context, string) error { return nil }
func (s bodySource) IsAvailable(string) bool            { return true }
func (s bodySource) Metrics() ArticleMetrics            { return ArticleMetrics{} }

// Read and Skip agree on positions when an article body is shorter than its
// declared size: both follow the declared layout that file offsets use.
func TestRarReaderReadAndSkipStayInStep(t *testing.T) {
	t.Parallel()
	source := bodySource{bodies: map[string][]byte{
		"short": []byte("abcdef"), // declared 10 bytes
		"full":  []byte("0123456789"),
	}}
	volumes := []*types.Volume{{Segments: []storage.NZBSegment{
		{MessageID: "short", Bytes: 10},
		{MessageID: "full", Bytes: 10},
	}}}

	read := newRarReader(t.Context(), source, volumes)
	head := make([]byte, 12)
	if _, err := io.ReadFull(read, head); err != nil {
		t.Fatal(err)
	}
	if want := append([]byte("abcdef\x00\x00\x00\x00"), "01"...); !bytes.Equal(head, want) {
		t.Fatalf("read %q, want %q", head, want)
	}

	skipped := newRarReader(t.Context(), source, volumes)
	if err := skipped.Skip(12); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*rarReader{read, skipped} {
		next := make([]byte, 1)
		if _, err := io.ReadFull(r, next); err != nil {
			t.Fatal(err)
		}
		if next[0] != '2' || r.Position() != 13 {
			t.Fatalf("after 12 bytes: next byte %q at position %d, want '2' at 13", next, r.Position())
		}
	}
}
