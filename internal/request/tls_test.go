package request_test

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
)

// Certificates are verified: a self-signed server is refused until its
// certificate is added to tls_ca_file.
func TestClientVerifiesCertificates(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	get := func(client *request.Client) error {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}

	if err := get(request.New(zerolog.Nop(), nil, request.WithMaxRetries(0))); err == nil {
		t.Fatal("a self-signed certificate was accepted by default")
	}

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	block := &pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}
	if err := os.WriteFile(caFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := (&config.Config{TLSCAFile: caFile}).TLSClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err = get(request.New(zerolog.Nop(), tlsConfig, request.WithMaxRetries(0))); err != nil {
		t.Fatalf("a certificate from tls_ca_file was refused: %v", err)
	}
}
