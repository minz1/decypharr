package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
)

const grabGoldenFile = "testdata/queue_record_grab_v1.bin"

func grabTestEntry() *Entry {
	return &Entry{
		InfoHash:       "abcdef0123456789abcdef0123456789abcdef01",
		Name:           "Show.S01",
		State:          EntryStateDownloading,
		Status:         debridTypes.TorrentStatusDownloading,
		Progress:       0.25,
		ActiveProvider: "realdebrid",
		AddedOn:        time.Unix(1_700_000_000, 0),
		Providers:      map[string]*ProviderEntry{"realdebrid": {Provider: "realdebrid", ID: "RD1"}},
		Grab:           []byte(`{"v":1,"phase":"provider"}`),
	}
}

// hasField reports whether the encoded message carries field num at its top level.
func hasField(t *testing.T, data []byte, num protowire.Number) bool {
	t.Helper()
	for len(data) > 0 {
		n, typ, l := protowire.ConsumeTag(data)
		if l < 0 {
			t.Fatalf("bad tag: %v", protowire.ParseError(l))
		}
		data = data[l:]
		if n == num {
			return true
		}
		vl := protowire.ConsumeFieldValue(n, typ, data)
		if vl < 0 {
			t.Fatalf("bad field %d: %v", n, protowire.ParseError(vl))
		}
		data = data[vl:]
	}
	return false
}

func TestQueueRecordKeepsTheGrabDocument(t *testing.T) {
	t.Parallel()
	s, _ := newQueueTestStorage(t)
	in := grabTestEntry()
	if err := s.UpdateQueuedGrab(in); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetQueuedGrab(in.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Grab, in.Grab) {
		t.Fatalf("GetQueuedGrab grab = %q, want %q", got.Grab, in.Grab)
	}
}

func TestQueueRecordWithoutAGrabDocumentReadsNil(t *testing.T) {
	t.Parallel()
	s, _ := newQueueTestStorage(t)
	in := grabTestEntry()
	in.Grab = nil
	if err := s.UpdateQueuedGrab(in); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetQueuedGrab(in.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if got.Grab != nil {
		t.Fatalf("grab = %q, want nil", got.Grab)
	}
}

// A writer outside the grab code changes the legacy fields without knowing the document, so its write must drop
// the document, as the previous binary's does; otherwise the document outlives the tuple it was written with.
func TestALegacyQueueWriteDropsTheGrabDocument(t *testing.T) {
	t.Parallel()
	s, _ := newQueueTestStorage(t)
	in := grabTestEntry()
	if err := s.UpdateQueuedGrab(in); err != nil {
		t.Fatal(err)
	}
	legacy, err := s.GetQueued(in.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Grab != nil {
		t.Fatalf("a legacy read carries the grab document %q", legacy.Grab)
	}
	listed, err := s.FilterQueued(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Grab != nil {
		t.Fatalf("FilterQueued = %+v", listed)
	}
	legacy.Grab = in.Grab // even a caller that copied it in cannot write it through the legacy path
	legacy.IsDownloading = false
	if err = s.UpdateQueue(legacy); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetQueuedGrab(in.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if got.Grab != nil {
		t.Fatalf("grab after a legacy write = %q", got.Grab)
	}
}

func TestLibraryRecordNeverCarriesTheGrabDocument(t *testing.T) {
	t.Parallel()
	s := openTestStorage(t, t.TempDir())
	defer s.Close()
	in := grabTestEntry()
	if err := s.AddOrUpdate(in); err != nil {
		t.Fatal(err)
	}
	data, err := s.entries.Get(in.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if hasField(t, data, grabField) {
		t.Fatal("library record carries the grab field")
	}
}

// TestPreviousBinaryReadsAndDropsTheGrabDocument pins the rollback contract on checked-in bytes: a binary that
// predates the grab document decodes the record through EntryProto, keeps every legacy field, and its next write
// (EntryToProto builds a fresh message) drops field 1000.
func TestPreviousBinaryReadsAndDropsTheGrabDocument(t *testing.T) {
	t.Parallel()
	in := grabTestEntry()
	encoded, err := encodeQueued(in, true)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("DECYPHARR_UPDATE_GOLDEN") != "" {
		if err = os.MkdirAll(filepath.Dir(grabGoldenFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(grabGoldenFile, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(grabGoldenFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(golden, encoded) {
		t.Fatal("the encoder no longer writes the checked-in bytes")
	}
	if !hasField(t, golden, grabField) {
		t.Fatal("golden record lacks the grab field")
	}

	var pb EntryProto
	if err = proto.Unmarshal(golden, &pb); err != nil {
		t.Fatalf("previous binary cannot decode: %v", err)
	}
	old := ProtoToEntry(&pb)
	if old.InfoHash != in.InfoHash || old.State != in.State || old.Status != in.Status ||
		old.Progress != in.Progress || old.ActiveProvider != in.ActiveProvider || !old.AddedOn.Equal(in.AddedOn) ||
		old.Providers["realdebrid"].ID != "RD1" {
		t.Fatalf("legacy fields changed: %+v", old)
	}
	rewritten, err := proto.Marshal(EntryToProto(old))
	if err != nil {
		t.Fatal(err)
	}
	if hasField(t, rewritten, grabField) {
		t.Fatal("the previous binary's write kept the grab field")
	}
	back, err := decodeQueued(rewritten, true)
	if err != nil {
		t.Fatal(err)
	}
	if back.Grab != nil {
		t.Fatalf("grab after the previous binary's write = %q", back.Grab)
	}
}
