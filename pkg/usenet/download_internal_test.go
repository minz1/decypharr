package usenet

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/fs/reader"
)

func TestDownloadDecryptsEncryptedFiles(t *testing.T) {
	t.Parallel()

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

	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() {
		ctx, cancel := task.Detached(t.Context(), 30*time.Second)
		defer cancel()
		_ = g.Stop(ctx)
	})
	host, port := srv.Addr()
	client, err := nntp.NewClient(g.Child("nntp"), &config.Config{Usenet: config.Usenet{
		Providers: []config.UsenetProvider{{Host: host, Port: port, MaxConnections: 4}},
	}}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	metaStore := &NZBStorage{metaDir: t.TempDir(), codec: testCodec(t), logger: zerolog.Nop()}
	if addErr := metaStore.AddNZB(&storage.NZB{ID: "nzb", Files: []storage.NZBFile{file}}); addErr != nil {
		t.Fatal(addErr)
	}
	bufferPools, poolsErr := reader.NewPools(g, 64<<20)
	if poolsErr != nil {
		t.Fatal(poolsErr)
	}
	scheduler, schedErr := reader.NewFetchScheduler(g.Child("scheduler"), 4)
	if schedErr != nil {
		t.Fatal(schedErr)
	}
	u := &Usenet{
		bufferPools:              bufferPools,
		fetchScheduler:           scheduler,
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
