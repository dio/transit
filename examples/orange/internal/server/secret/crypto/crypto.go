package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// ErrAuthFailed is returned when the Poly1305 tag or AAD does not match.
var ErrAuthFailed = errors.New("secretstore/crypto: authentication failed")

// Wire format constants for the self-contained ciphertext blob:
//
//	version_byte[1] | hkdf_salt[32] | nonce[24] | ciphertext[N+16]
const (
	versionByte byte = 0x01 // XChaCha20-Poly1305 + HKDF-SHA256
	saltSize         = 32
	nonceSize        = chacha20poly1305.NonceSizeX // 24
	headerSize       = 1 + saltSize + nonceSize    // 57
)

// Context strings for HKDF domain separation.
const (
	ContextWrap = "secret-storage/v1/wrap"
	ContextData = "secret-storage/v1/data"
)

// Encryptor seals and opens byte payloads using XChaCha20-Poly1305 with
// a per-operation HKDF-SHA256 subkey.
type Encryptor interface {
	// Seal derives a subkey, generates a random nonce, and seals plaintext.
	// Returns a self-contained blob (version_byte | hkdf_salt | nonce | ct).
	Seal(context string, key, plaintext, aad []byte) (blob []byte, err error)

	// Open derives the subkey from the blob's embedded hkdf_salt and opens
	// the ciphertext. Returns ErrAuthFailed on tag or AAD mismatch.
	Open(context string, key, blob, aad []byte) (plaintext []byte, err error)
}

// XChaCha20Encryptor is the production Encryptor using XChaCha20-Poly1305
// and HKDF-SHA256 key derivation.
type XChaCha20Encryptor struct{}

// New returns the default XChaCha20-Poly1305 + HKDF-SHA256 encryptor.
func New() Encryptor { return XChaCha20Encryptor{} }

func (XChaCha20Encryptor) Seal(context string, key, plaintext, aad []byte) ([]byte, error) {
	salt := make([]byte, saltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("secretstore/crypto: rand salt: %w", err)
	}

	subkey, err := deriveKey(key, salt, context)
	if err != nil {
		return nil, err
	}
	defer zeroize(subkey)

	aead, err := chacha20poly1305.NewX(subkey)
	if err != nil {
		return nil, fmt.Errorf("secretstore/crypto: new aead: %w", err)
	}

	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secretstore/crypto: rand nonce: %w", err)
	}

	ct := aead.Seal(nil, nonce, plaintext, aad)

	blob := make([]byte, headerSize+len(ct))
	blob[0] = versionByte
	copy(blob[1:], salt)
	copy(blob[1+saltSize:], nonce)
	copy(blob[headerSize:], ct)
	return blob, nil
}

func (XChaCha20Encryptor) Open(context string, key, blob, aad []byte) ([]byte, error) {
	if len(blob) < headerSize+chacha20poly1305.Overhead {
		return nil, fmt.Errorf("secretstore/crypto: blob too short")
	}
	if blob[0] != versionByte {
		return nil, fmt.Errorf("secretstore/crypto: unknown version byte 0x%02x", blob[0])
	}

	salt := blob[1 : 1+saltSize]
	nonce := blob[1+saltSize : headerSize]
	ct := blob[headerSize:]

	subkey, err := deriveKey(key, salt, context)
	if err != nil {
		return nil, err
	}
	defer zeroize(subkey)

	aead, err := chacha20poly1305.NewX(subkey)
	if err != nil {
		return nil, fmt.Errorf("secretstore/crypto: new aead: %w", err)
	}

	plaintext, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, ErrAuthFailed
	}
	return plaintext, nil
}

// deriveKey returns a 32-byte subkey via HKDF-SHA256.
func deriveKey(ikm, salt []byte, info string) ([]byte, error) {
	r := hkdf.New(sha256.New, ikm, salt, []byte(info))
	subkey := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(r, subkey); err != nil {
		return nil, fmt.Errorf("secretstore/crypto: hkdf: %w", err)
	}
	return subkey, nil
}

// Zeroize clears sensitive key material from memory.
// Call via defer immediately after allocation:
//
//	key := make([]byte, 32)
//	defer crypto.Zeroize(key)
func Zeroize(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func zeroize(b []byte) { Zeroize(b) }

// SHA256Hex returns the lowercase hex-encoded SHA-256 digest of b.
func SHA256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return fmt.Sprintf("%x", h)
}
