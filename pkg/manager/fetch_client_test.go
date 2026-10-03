package manager_test

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

// NZB and .torrent fetches trust the configured tls_ca_file, so an indexer
// behind a private CA works; they used to verify against the system roots
// only.
func TestFetchClientTrustsConfiguredCA(t *testing.T) {
	t.Parallel()
	indexer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<nzb/>"))
	}))
	t.Cleanup(indexer.Close)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: indexer.Certificate().Raw})
	if err := os.WriteFile(caFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, _ := managertest.New(t, func(c *config.Config) { c.TLSCAFile = caFile })
	_, body, err := utils.DownloadFile(mgr.FetchClient(), indexer.URL+"/get.nzb")
	if err != nil {
		t.Fatalf("download from an indexer signed by tls_ca_file: %v", err)
	}
	if string(body) != "<nzb/>" {
		t.Fatalf("body = %q", body)
	}
}
