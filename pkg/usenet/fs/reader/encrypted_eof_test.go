package reader

import (
	"errors"
	"io"
	"testing"
)

// TestEncryptedReadPastEOFReportsEOF: RAR5 AES data is always block-aligned,
// so a read crossing the end of such a file must report io.EOF with the
// short count, exactly like the plain path; (n < len(p), nil) breaks the
// io.ReaderAt contract.
func TestEncryptedReadPastEOFReportsEOF(t *testing.T) {
	t.Parallel()
	sr := newTestReader(t, 2) // 2000 bytes: a multiple of the AES block
	prefillSegments(t, sr, 0, 1)
	sr.encryption = EncryptionConfig{Enabled: true, Key: make([]byte, 32), IV: make([]byte, 16)}

	buf := make([]byte, 64)
	n, err := sr.ReadAt(buf, sr.Size()-16)
	if n != 16 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt across EOF = (%d, %v), want (16, io.EOF)", n, err)
	}

	n, err = sr.ReadAt(buf[:16], sr.Size()-16)
	if n != 16 || err != nil {
		t.Fatalf("ReadAt ending exactly at EOF = (%d, %v), want (16, nil)", n, err)
	}
}
