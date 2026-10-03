package parser

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/javi11/sevenzip"
)

func TestSplitStreamable7zEntriesDropsPackedData(t *testing.T) {
	t.Parallel()
	rarFiles, plainFiles := splitStreamable7zEntries([]sevenzip.FileInfo{
		{Name: "movie.mkv"},
		{Name: "packed.mkv", Compressed: true},
		{Name: "secret.mkv", Encrypted: true},
		{Name: "movie.rar"},
		{Name: "movie.r00", Compressed: true},
		{Name: "movie.r01", Encrypted: true},
	})
	if len(plainFiles) != 1 || plainFiles[0].Name != "movie.mkv" {
		t.Fatalf("plain files = %+v, want only the stored movie.mkv", plainFiles)
	}
	if len(rarFiles) != 1 || rarFiles[0].Name != "movie.rar" {
		t.Fatalf("RAR volumes = %+v, want only the stored movie.rar", rarFiles)
	}
}

func zipCentralEntry(name string, flags, method uint16) []byte {
	entry := make([]byte, 46, 46+len(name))
	binary.LittleEndian.PutUint32(entry[0:], ZIPCentralDirectoryHeaderSig)
	binary.LittleEndian.PutUint16(entry[8:], flags)
	binary.LittleEndian.PutUint16(entry[10:], method)
	binary.LittleEndian.PutUint32(entry[20:], 100) // compressed size
	binary.LittleEndian.PutUint32(entry[24:], 100) // uncompressed size
	binary.LittleEndian.PutUint16(entry[28:], uint16(len(name)))
	return append(entry, name...)
}

func TestZIPEncryptedStoredEntryIsNotStreamable(t *testing.T) {
	t.Parallel()
	p := &ZIPParser{}
	plain, err := p.parseCentralDirEntry(bytes.NewReader(zipCentralEntry("movie.mkv", 0, ZIPStoreMethod)))
	if err != nil || !plain.IsStored {
		t.Fatalf("plain stored entry = %+v, err = %v", plain, err)
	}
	encrypted, err := p.parseCentralDirEntry(bytes.NewReader(zipCentralEntry("movie.mkv", 1, ZIPStoreMethod)))
	if err != nil {
		t.Fatal(err)
	}
	if encrypted.IsStored {
		t.Fatal("ZipCrypto-encrypted entry reported as stored; its bytes are ciphertext")
	}
}
