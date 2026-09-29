package crypto_test

import (
	"errors"
	"testing"

	"github.com/sirrobot01/decypharr/internal/crypto"
)

func encryptionHeader(kdfCount byte, withCheck bool) []byte {
	flags := byte(0)
	if withCheck {
		flags = 1
	}
	data := append([]byte{0, flags, kdfCount}, make([]byte, 16)...)
	if withCheck {
		data = append(data, make([]byte, 12)...)
	}
	return data
}

func TestParseEncryptionHeaderRejectsHugeKdfCount(t *testing.T) {
	t.Parallel()
	// A header-supplied count above 24 would make DeriveKeys run 2^count
	// PBKDF2 rounds; unrar rejects it and so must we.
	for _, kdf := range []byte{crypto.MaxKdfCount + 1, 40, 255} {
		if _, err := crypto.ParseEncryptionHeader(
			encryptionHeader(kdf, false),
		); !errors.Is(
			err,
			crypto.ErrInvalidData,
		) {
			t.Errorf("kdfCount %d: err = %v, want ErrInvalidData", kdf, err)
		}
	}
	h, err := crypto.ParseEncryptionHeader(encryptionHeader(crypto.MaxKdfCount, true))
	if err != nil {
		t.Fatalf("kdfCount %d: %v", crypto.MaxKdfCount, err)
	}
	if h.KdfCount != crypto.MaxKdfCount || !h.HasPwCheck || len(h.PwCheck) != 12 || len(h.Salt) != 16 {
		t.Fatalf("unexpected header: %+v", h)
	}
}

func TestDeriveKeysPasswordCheck(t *testing.T) {
	t.Parallel()
	salt := []byte("0123456789abcdef")
	keys := crypto.DeriveKeys([]byte("secret"), salt, 4)
	if len(keys.Key) != crypto.AESKeySize || len(keys.PwCheck) != crypto.PwCheckSize+4 {
		t.Fatalf("key sizes: key=%d pwcheck=%d", len(keys.Key), len(keys.PwCheck))
	}
	if !crypto.VerifyPassword(keys, keys.PwCheck) {
		t.Fatal("password check does not verify against itself")
	}
	if crypto.VerifyPassword(crypto.DeriveKeys([]byte("wrong"), salt, 4), keys.PwCheck) {
		t.Fatal("wrong password verified")
	}
}
