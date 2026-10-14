package usenet

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/customerror"
)

func TestDeleteReportsMissingMetaAsManifestMissing(t *testing.T) {
	t.Parallel()
	metaDir := filepath.Join(t.TempDir(), "meta")
	nzbStorage, err := NewNZBStorage(metaDir, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	u := &Usenet{nzbStorage: nzbStorage, logger: zerolog.Nop()}

	if delErr := u.Delete("absent"); !errors.Is(delErr, customerror.ErrUsenetManifestMissing) {
		t.Fatalf("missing meta: %v", delErr)
	}
	if rmErr := os.RemoveAll(metaDir); rmErr != nil {
		t.Fatal(rmErr)
	}
	if delErr := u.Delete("absent"); delErr == nil || errors.Is(delErr, customerror.ErrUsenetManifestMissing) {
		t.Fatalf("unavailable meta dir must not read as already gone: %v", delErr)
	}
}
