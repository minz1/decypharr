package config

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
)

func TestProviderTLSConfigServerName(t *testing.T) {
	t.Parallel()
	base := &tls.Config{MinVersion: tls.VersionTLS13}
	if got := ProviderTLSConfig(base, UsenetProvider{Host: "eu.news.example"}); got.ServerName != "eu.news.example" ||
		got.MinVersion != tls.VersionTLS13 || got.InsecureSkipVerify {
		t.Fatalf("default = %+v", got)
	}
	got := ProviderTLSConfig(nil, UsenetProvider{Host: "10.0.0.5", TLSServerName: "news.example"})
	if got.ServerName != "news.example" || got.MinVersion != tls.VersionTLS12 {
		t.Fatalf("override = %+v", got)
	}
	if base.ServerName != "" {
		t.Fatal("ProviderTLSConfig changed the shared base")
	}
}

func TestInvalidTLSCAFileFailsLoad(t *testing.T) {
	t.Parallel()
	notPEM := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{notPEM, filepath.Join(t.TempDir(), "missing.pem")} {
		if _, err := Load(t.TempDir(), MapEnv(map[string]string{"DECYPHARR_TLS_CA_FILE": file})); err == nil {
			t.Errorf("tls_ca_file %s loaded", file)
		}
	}
}
