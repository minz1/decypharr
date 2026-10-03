package types

import (
	"crypto/tls"

	"github.com/rs/zerolog"
)

// ProviderOptions carries what every debrid provider takes from the
// application besides its own config.Debrid section.
type ProviderOptions struct {
	// Retries is how many times a failed API call is retried.
	Retries int
	// ValidateFile reports whether a file may be imported (allowed
	// extension, size limits, samples). It reads the live configuration, so
	// edits apply without a restart. Nil allows every file.
	ValidateFile func(name string, size int64) error
	// Logger is the provider's logger.
	Logger zerolog.Logger
	// TLSConfig is the verified TLS configuration for API and download
	// requests. Nil means the system roots.
	TLSConfig *tls.Config
}

// FileAllowed applies ValidateFile, allowing every file when it is nil.
func (o ProviderOptions) FileAllowed(name string, size int64) error {
	if o.ValidateFile == nil {
		return nil
	}
	return o.ValidateFile(name, size)
}
