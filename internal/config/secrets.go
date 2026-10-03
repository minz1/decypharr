package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Secret holds a credential in memory. It never prints its value (String
// and GoString redact it) and is never written into config.json: the fields
// that hold one are tagged json:"-" and stored in secrets.json by
// writeSecrets. Read it with Reveal.
type Secret struct {
	value string
}

// NewSecret wraps value.
func NewSecret(value string) Secret { return Secret{value: value} }

// Reveal returns the secret's value.
func (s Secret) Reveal() string { return s.value }

// IsZero reports whether the secret is unset.
func (s Secret) IsZero() bool { return s.value == "" }

func (s Secret) String() string {
	if s.IsZero() {
		return ""
	}
	return "[redacted]"
}

// GoString keeps %#v from printing the value.
func (s Secret) GoString() string { return "config.Secret{" + s.String() + "}" }

// SecretsFile is the path of secrets.json, which holds the secrets kept out
// of config.json. It is written 0600.
func (c *Config) SecretsFile() string {
	return filepath.Join(c.meta.dir, "secrets.json")
}

// secretsRecord is the secrets.json layout. Its MarshalJSON is the one place
// these secrets are encoded with their values, for the 0600 secrets file.
type secretsRecord struct {
	sessionSecret Secret
}

// secretsJSON is the on-disk form of secretsRecord.
type secretsJSON struct {
	SessionSecret string `json:"session_secret,omitempty"`
}

// MarshalJSON writes the secrets in clear, for secrets.json only.
func (r secretsRecord) MarshalJSON() ([]byte, error) {
	return json.Marshal(secretsJSON{SessionSecret: r.sessionSecret.Reveal()})
}

// readSecrets loads secrets.json into c. Earlier versions kept the session
// secret in config.json (raw is its content); it is read from there when
// secrets.json does not have it, and Save moves it.
func (c *Config) readSecrets(raw []byte) error {
	var legacy secretsJSON
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return fmt.Errorf("error parsing config JSON: %w", err)
		}
	}
	var stored secretsJSON
	data, err := os.ReadFile(c.SecretsFile())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("error reading secrets file: %w", err)
	default:
		if unmarshalErr := json.Unmarshal(data, &stored); unmarshalErr != nil {
			return fmt.Errorf("error parsing secrets file: %w", unmarshalErr)
		}
	}
	if secret := stored.SessionSecret; secret != "" {
		c.SessionSecret = NewSecret(secret)
	} else if secret = legacy.SessionSecret; secret != "" {
		c.SessionSecret = NewSecret(secret)
	}
	return nil
}

// writeSecrets saves the secrets to secrets.json (0600).
func (c *Config) writeSecrets() error {
	data, err := json.MarshalIndent(secretsRecord{sessionSecret: c.SessionSecret}, "", "  ")
	if err != nil {
		return err
	}
	return c.writeFile(c.SecretsFile(), data)
}
