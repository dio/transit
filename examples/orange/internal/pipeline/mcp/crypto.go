package mcp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
)

const (
	cryptoSaltSize   = 16
	cryptoKeyLength  = 32
	cryptoIterations = 10_000
)

type sessionCrypto interface {
	Encrypt(plaintext []byte) (string, error)
	Decrypt(token string) ([]byte, error)
}

type pbkdf2AESGCM struct {
	key string
}

func newSessionCrypto(primary string, fallbacks ...string) sessionCrypto {
	c := sessionCrypto(&pbkdf2AESGCM{key: primary})
	for i := len(fallbacks) - 1; i >= 0; i-- {
		if fallbacks[i] == "" {
			continue
		}
		c = fallbackCrypto{
			primary:  c,
			fallback: &pbkdf2AESGCM{key: fallbacks[i]},
		}
	}
	return c
}

func (c *pbkdf2AESGCM) Encrypt(plaintext []byte) (string, error) {
	salt := make([]byte, cryptoSaltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", err
	}
	key, err := c.deriveKey(salt)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	out := make([]byte, 0, len(salt)+len(nonce)+len(plaintext)+gcm.Overhead())
	out = append(out, salt...)
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, plaintext, nil)
	return base64.RawURLEncoding.EncodeToString(out), nil
}

func (c *pbkdf2AESGCM) Decrypt(token string) ([]byte, error) {
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, err
	}
	if len(data) < cryptoSaltSize {
		return nil, fmt.Errorf("encrypted envelope too short")
	}
	salt := data[:cryptoSaltSize]
	key, err := c.deriveKey(salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(data) < cryptoSaltSize+gcm.NonceSize() {
		return nil, fmt.Errorf("encrypted envelope missing nonce")
	}
	nonce := data[cryptoSaltSize : cryptoSaltSize+gcm.NonceSize()]
	ciphertext := data[cryptoSaltSize+gcm.NonceSize():]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func (c *pbkdf2AESGCM) deriveKey(salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, c.key, salt, cryptoIterations, cryptoKeyLength)
}

type fallbackCrypto struct {
	primary  sessionCrypto
	fallback sessionCrypto
}

func (c fallbackCrypto) Encrypt(plaintext []byte) (string, error) {
	return c.primary.Encrypt(plaintext)
}

func (c fallbackCrypto) Decrypt(token string) ([]byte, error) {
	plaintext, err := c.primary.Decrypt(token)
	if err == nil {
		return plaintext, nil
	}
	if c.fallback == nil {
		return nil, err
	}
	return c.fallback.Decrypt(token)
}
