package parser

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/sirrobot01/decypharr/internal/crypto"
)

type rar5Encryption struct {
	Encrypted bool
	Key, IV   []byte
}

// parseRAR5Extra reads each record within its declared size.
// Record sizes include the variable-length type field.
// Format: https://www.rarlab.com/technote.htm
func parseRAR5Extra(data []byte, password string) (rar5Encryption, error) {
	var encryption rar5Encryption
	for len(data) > 0 {
		size, n := binary.Uvarint(data)
		if n <= 0 {
			return rar5Encryption{}, fmt.Errorf("invalid RAR5 extra record size")
		}
		data = data[n:]
		if size == 0 || size > uint64(len(data)) {
			return rar5Encryption{}, io.ErrUnexpectedEOF
		}
		record := bytes.NewReader(data[:size])
		data = data[size:]
		kind, err := binary.ReadUvarint(record)
		if err != nil {
			return rar5Encryption{}, fmt.Errorf("RAR5 extra record type: %w", err)
		}
		if kind != RAR5ExtraTypeEncryption {
			continue
		}
		if encryption.Encrypted {
			return rar5Encryption{}, fmt.Errorf("duplicate RAR5 encryption record")
		}
		if encryption, err = parseRAR5EncryptionRecord(record, password); err != nil {
			return rar5Encryption{}, err
		}
	}
	return encryption, nil
}

// RAR5 file encryption record layout.
const (
	rar5SaltSize       = 16
	rar5IVSize         = 16
	rar5PwCheckSize    = 12
	rar5FlagPwCheck    = 0x01
	maxRAR5KDFExponent = 24 // limit key derivation to 2^24 rounds per file
)

// parseRAR5EncryptionRecord reads an encryption record after its type and
// derives the file key when a password is given.
func parseRAR5EncryptionRecord(record *bytes.Reader, password string) (rar5Encryption, error) {
	version, err := binary.ReadUvarint(record)
	if err != nil || version != 0 {
		return rar5Encryption{}, fmt.Errorf("invalid RAR5 encryption version")
	}
	flags, err := binary.ReadUvarint(record)
	if err != nil {
		return rar5Encryption{}, fmt.Errorf("RAR5 encryption flags: %w", err)
	}
	kdf, err := record.ReadByte()
	if err != nil || kdf > maxRAR5KDFExponent {
		return rar5Encryption{}, fmt.Errorf("invalid RAR5 key derivation count")
	}
	var salt [rar5SaltSize]byte
	if _, readFullErr := io.ReadFull(record, salt[:]); readFullErr != nil {
		return rar5Encryption{}, fmt.Errorf("RAR5 salt: %w", readFullErr)
	}
	iv := make([]byte, rar5IVSize)
	if _, readFullErr := io.ReadFull(record, iv); readFullErr != nil {
		return rar5Encryption{}, fmt.Errorf("RAR5 IV: %w", readFullErr)
	}
	if flags&rar5FlagPwCheck != 0 {
		var check [rar5PwCheckSize]byte
		if _, readFullErr := io.ReadFull(record, check[:]); readFullErr != nil {
			return rar5Encryption{}, fmt.Errorf("RAR5 password check: %w", readFullErr)
		}
	}
	encryption := rar5Encryption{Encrypted: true, IV: iv}
	if password != "" {
		encryption.Key = crypto.DeriveKeys([]byte(password), salt[:], int(kdf)).Key
	}
	return encryption, nil
}
