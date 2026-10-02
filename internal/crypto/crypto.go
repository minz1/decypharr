// Package crypto Package rar provides RAR5 encryption/decryption utilities.
// Implements AES-256-CBC decryption with PBKDF2-HMAC-SHA256 key derivation
// as specified in the RAR 5.0 archive format.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
)

const (
	// BlockSize AES block size.
	BlockSize = 16

	// AESKeySize Key sizes.
	AESKeySize = 32 // AES-256

	// MaxPbkdf2Salt RAR5 constants.
	MaxPbkdf2Salt = 64
	PwCheckSize   = 8
	MaxKdfCount   = 24
)

const (
	saltSize         = 16 // salt length in the encryption header
	pwCheckFieldSize = 12 // stored check value: PwCheckSize bytes + 4-byte SHA-256 prefix
	pwCheckSumSize   = pwCheckFieldSize - PwCheckSize
	derivedKeyCount  = 3    // key, check key, password check
	extraKeyRounds   = 16   // additional PBKDF2 rounds for the 2nd and 3rd value
	vintValueMask    = 0x7F // low 7 bits of a vint byte
	vintMoreFlag     = 0x80 // continuation bit of a vint byte
	flagHasPwCheck   = 0x0001
	// minHeaderSize is version + flags + kdfCount + salt.
	minHeaderSize = 3 + saltSize
)

var (
	ErrBadPassword    = errors.New("rar: incorrect password")
	ErrInvalidKeySize = errors.New("rar: invalid key size")
	ErrInvalidIVSize  = errors.New("rar: invalid IV size")
	ErrInvalidData    = errors.New("rar: invalid encrypted data")
)

// DerivedKeys contains the keys derived from a password using RAR5's PBKDF2.
type DerivedKeys struct {
	Key      []byte // AES-256 key for decryption (32 bytes)
	CheckKey []byte // Key for checksum verification (32 bytes)
	PwCheck  []byte // Password verification value (12 bytes)
}

// DeriveKeys derives encryption keys from password using RAR5's PBKDF2-HMAC-SHA256.
// kdfCount is the log2 of iterations (actual iterations = 2^kdfCount).
// This implementation matches RAR5's calcKeys50 algorithm.
func DeriveKeys(password, salt []byte, kdfCount int) *DerivedKeys {
	if len(salt) > MaxPbkdf2Salt {
		salt = salt[:MaxPbkdf2Salt]
	}

	// Calculate actual iteration count
	iterations := 1 << uint(kdfCount)

	// Initialize HMAC with password
	prf := hmac.New(sha256.New, password)
	prf.Write(salt)
	prf.Write([]byte{0, 0, 0, 1}) // Counter = 1

	// Initial values
	t := prf.Sum(nil)
	u := make([]byte, len(t))
	copy(u, t)

	iterations--

	// Derive 3 keys with different iteration counts
	keys := make([][]byte, derivedKeyCount)
	iterCounts := []int{iterations, extraKeyRounds, extraKeyRounds}

	for i, iter := range iterCounts {
		for iter > 0 {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range u {
				t[j] ^= u[j]
			}
			iter--
		}
		keys[i] = make([]byte, len(t))
		copy(keys[i], t)
	}

	// Build password check value
	pwcheck := make([]byte, len(keys[2]))
	copy(pwcheck, keys[2])

	// XOR fold the password check
	for i, v := range pwcheck[PwCheckSize:] {
		pwcheck[i&(PwCheckSize-1)] ^= v
	}
	// Add SHA256 checksum (first 4 bytes) of the folded value.
	sum := sha256.Sum256(pwcheck[:PwCheckSize])
	copy(pwcheck[PwCheckSize:], sum[:pwCheckSumSize])
	pwcheck = pwcheck[:pwCheckFieldSize]

	return &DerivedKeys{
		Key:      keys[0],
		CheckKey: keys[1],
		PwCheck:  pwcheck,
	}
}

// VerifyPassword checks if the password is correct by comparing password check values.
// expectedCheck is the 12-byte value stored in the encryption header.
func VerifyPassword(keys *DerivedKeys, expectedCheck []byte) bool {
	if len(expectedCheck) != len(keys.PwCheck) {
		return false
	}
	// Constant-time comparison
	var diff byte
	for i := range expectedCheck {
		diff |= expectedCheck[i] ^ keys.PwCheck[i]
	}
	return diff == 0
}

// NewDecrypter creates an AES-256-CBC decrypter with the given key and IV.
func NewDecrypter(key, iv []byte) (cipher.BlockMode, error) {
	if len(key) != AESKeySize {
		return nil, ErrInvalidKeySize
	}
	if len(iv) != BlockSize {
		return nil, ErrInvalidIVSize
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	return cipher.NewCBCDecrypter(block, iv), nil
}

// DecryptBlock decrypts a single block of data using AES-256-CBC.
// The data length must be a multiple of 16 (AES block size).
// Decryption is done in-place.
func DecryptBlock(data, key, iv []byte) error {
	if len(data)%BlockSize != 0 {
		return ErrInvalidData
	}

	mode, err := NewDecrypter(key, iv)
	if err != nil {
		return err
	}

	mode.CryptBlocks(data, data)
	return nil
}

// EncryptionHeader contains RAR5 encryption header data.
type EncryptionHeader struct {
	Version    int    // Encryption version (should be 0)
	KdfCount   int    // Log2 of PBKDF2 iterations
	Salt       []byte // Salt for key derivation (16 bytes)
	PwCheck    []byte // Password verification value (12 bytes, optional)
	HasPwCheck bool   // Whether password check is present
}

// ParseEncryptionHeader parses a RAR5 encryption header.
// Format: version (vint) + flags (vint) + kdfCount (1 byte) + salt (16 bytes) + [pwCheck (12 bytes)].
func ParseEncryptionHeader(data []byte) (*EncryptionHeader, error) {
	if len(data) < minHeaderSize {
		return nil, ErrInvalidData
	}

	// Read version (should be 0)
	version := int(data[0] & vintValueMask)
	pos := 1
	if data[0]&vintMoreFlag != 0 {
		// Multi-byte vint, but version should be 0
		return nil, ErrInvalidData
	}

	// Read flags
	flags := int(data[pos] & vintValueMask)
	pos++
	if data[pos-1]&vintMoreFlag != 0 {
		return nil, ErrInvalidData
	}

	// Read KDF count (1 byte)
	if pos >= len(data) {
		return nil, ErrInvalidData
	}
	kdfCount := int(data[pos])
	pos++
	// The count comes from the archive: reject values unrar also rejects, or
	// DeriveKeys would spin through up to 2^255 PBKDF2 rounds.
	if kdfCount > MaxKdfCount {
		return nil, ErrInvalidData
	}

	// Read salt (16 bytes)
	if pos+saltSize > len(data) {
		return nil, ErrInvalidData
	}
	salt := make([]byte, saltSize)
	copy(salt, data[pos:pos+saltSize])
	pos += saltSize

	header := &EncryptionHeader{
		Version:  version,
		KdfCount: kdfCount,
		Salt:     salt,
	}

	// Read password check if present (flag 0x0001)
	if flags&flagHasPwCheck != 0 {
		if pos+pwCheckFieldSize > len(data) {
			return nil, ErrInvalidData
		}
		header.PwCheck = make([]byte, pwCheckFieldSize)
		copy(header.PwCheck, data[pos:pos+pwCheckFieldSize])
		header.HasPwCheck = true
	}

	return header, nil
}
