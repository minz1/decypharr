package buffer

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestPunchHoleReclaims checks that the platform's punch implementation does
// what the Buffer's disk accounting assumes it does. Before this had per-OS
// implementations, punchHole was a no-op returning nil everywhere but Linux,
// so Discard unpublished ranges whose bytes were still on disk — reclaiming
// nothing and forcing a re-download. A silent regression to that shape is
// exactly what this test exists to catch.
func TestPunchHoleReclaims(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "punch.bin")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if prepareSparseErr := prepareSparse(f); prepareSparseErr != nil {
		t.Skipf("sparse files unsupported here: %v", prepareSparseErr)
	}

	const size = 8 << 20
	payload := bytes.Repeat([]byte{0xAB}, size)
	if _, writeAtErr := f.WriteAt(payload, 0); writeAtErr != nil {
		t.Fatal(writeAtErr)
	}
	if syncErr := f.Sync(); syncErr != nil {
		t.Fatal(syncErr)
	}
	before := allocatedBlocks(t, path)

	if punchHoleErr := punchHole(f, 0, size); punchHoleErr != nil {
		t.Skipf("punch unsupported here: %v", punchHoleErr)
	}
	if syncErr := f.Sync(); syncErr != nil {
		t.Fatal(syncErr)
	}

	// The range must read back as a hole, and the logical size must survive:
	// the Buffer writes every segment at a fixed offset, so a punch that
	// truncated the file would invalidate every address past the hole.
	got := make([]byte, size)
	if _, readAtErr := f.ReadAt(got, 0); readAtErr != nil {
		t.Fatalf("read after punch: %v", readAtErr)
	}
	if !bytes.Equal(got, make([]byte, size)) {
		t.Fatal("punched range still holds data")
	}
	if st, statErr := f.Stat(); statErr != nil || st.Size() != size {
		t.Fatalf("logical size changed by punch: %v size=%v", statErr, st.Size())
	}

	if after := allocatedBlocks(t, path); before > 0 && after >= before {
		t.Fatalf("punch reclaimed no blocks: before=%d after=%d", before, after)
	}
}
