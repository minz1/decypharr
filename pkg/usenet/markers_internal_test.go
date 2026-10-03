package usenet

import (
	"errors"
	"os"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestMarkAsFailedWithoutPathLeavesWorkingDirectoryAlone(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".processing", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	u := &Usenet{
		nzbStorage: &NZBStorage{metaDir: t.TempDir(), codec: testCodec(t), logger: zerolog.Nop()},
		logger:     zerolog.Nop(),
	}
	if err := u.markAsFailed(&storage.NZB{ID: "queued"}, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(".processing"); err != nil {
		t.Fatalf("unrelated ./.processing was removed: %v", err)
	}
}
