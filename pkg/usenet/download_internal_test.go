package usenet

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

//nolint:paralleltest // points the process-wide config singleton at a temp dir
func TestDownloadDecryptsEncryptedFiles(t *testing.T) {
	config.SetConfigPath(t.TempDir())
	config.Reset()
	t.Cleanup(config.Reset)

	const segSize = 32 << 10
	plain := nntpd.Pattern(0, 2*segSize)
	key, iv := bytes.Repeat([]byte{3}, 32), bytes.Repeat([]byte{9}, 16)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	encrypted := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(encrypted, plain)

	srv, err := nntpd.New(nntpd.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	file := storage.NZBFile{
		Name:          "movie.mkv",
		Size:          int64(len(plain)),
		FileType:      storage.NZBFileTypeRar,
		IsStored:      true,
		IsEncrypted:   true,
		EncryptionKey: key,
		EncryptionIV:  iv,
	}
	for i := range 2 {
		off := int64(i * segSize)
		id := fmt.Sprintf("<enc-%d@nntpd>", i)
		srv.AddArticle(id, nntpd.Encode(encrypted[off:off+segSize], "movie.rar", i+1, int64(len(plain)), off))
		file.Segments = append(file.Segments, storage.NZBSegment{
			Number: i + 1, MessageID: id, Bytes: segSize, StartOffset: off, EndOffset: off + segSize - 1,
		})
	}

	host, port := srv.Addr()
	client, err := nntp.NewClient(&config.Config{Usenet: config.Usenet{
		Providers: []config.UsenetProvider{{Host: host, Port: port, MaxConnections: 4}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	metaStore := &NZBStorage{metaDir: t.TempDir(), logger: zerolog.Nop()}
	if addErr := metaStore.AddNZB(&storage.NZB{ID: "nzb", Files: []storage.NZBFile{file}}); addErr != nil {
		t.Fatal(addErr)
	}
	u := &Usenet{
		nntp:                     client,
		nzbStorage:               metaStore,
		logger:                   zerolog.Nop(),
		maxConnections:           4,
		processingMaxConnections: 4,
		prefetchSize:             1 << 20,
	}

	var out bytes.Buffer
	var reported int64
	if downloadErr := u.Download(t.Context(), "nzb", "movie.mkv", &out, func(done, _ int64) {
		reported = done
	}); downloadErr != nil {
		t.Fatal(downloadErr)
	}
	if !bytes.Equal(out.Bytes(), plain) {
		t.Fatalf("downloaded %d bytes that do not match the decrypted plaintext", out.Len())
	}
	if reported != int64(len(plain)) {
		t.Fatalf("progress reported %d bytes, want %d", reported, len(plain))
	}
}
