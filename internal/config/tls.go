package config

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

var errNoCertificates = errors.New("no PEM certificates found")

// TLSClientConfig returns the TLS settings for outgoing connections (debrid,
// *arr, usenet): TLS 1.2 or newer, verified against the system roots plus
// any certificates in TLSCAFile. Certificates are always verified; a host
// whose certificate is self-signed or issued by a private CA is trusted by
// adding that CA to TLSCAFile.
func (c *Config) TLSClientConfig() (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.TLSCAFile == "" {
		return tlsConfig, nil
	}
	pem, err := os.ReadFile(c.TLSCAFile)
	if err != nil {
		return nil, fmt.Errorf("read tls_ca_file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("tls_ca_file %s: %w", c.TLSCAFile, errNoCertificates)
	}
	tlsConfig.RootCAs = pool
	return tlsConfig, nil
}

// ProviderTLSConfig specializes base (nil: the system roots) for one usenet
// provider: the certificate must name TLSServerName when set, else the
// provider's host.
func ProviderTLSConfig(base *tls.Config, provider UsenetProvider) *tls.Config {
	if base == nil {
		base = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tlsConfig := base.Clone()
	tlsConfig.ServerName = provider.TLSServerName
	if tlsConfig.ServerName == "" {
		tlsConfig.ServerName = provider.Host
	}
	return tlsConfig
}
