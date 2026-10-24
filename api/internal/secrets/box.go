// Package secrets encrypts endpoint signing secrets at rest with AES-256-GCM.
// The additional authenticated data binds ciphertext to a tenant, endpoint,
// secret row, and KEK version so rows cannot be transplanted.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

const (
	KeySize   = 32
	NonceSize = 12
)

// Box encrypts and decrypts short secret values.
type Box struct {
	aead    cipher.AEAD
	version int
}

// New validates a 32-byte KEK and returns a Box.
func New(kek []byte, version int) (*Box, error) {
	if len(kek) != KeySize {
		return nil, fmt.Errorf("KEK must be %d bytes", KeySize)
	}
	if version < 1 {
		return nil, errors.New("KEK version must be >= 1")
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	return &Box{aead: aead, version: version}, nil
}

// Version returns the KEK version recorded with new ciphertext.
func (b *Box) Version() int { return b.version }

// Seal returns ciphertext (including the GCM tag) and a random nonce.
func (b *Box) Seal(plaintext, aad []byte) ([]byte, []byte, error) {
	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}
	return b.aead.Seal(nil, nonce, plaintext, aad), nonce, nil
}

// Open authenticates and decrypts ciphertext.
func (b *Box) Open(ciphertext, nonce, aad []byte) ([]byte, error) {
	if len(nonce) != NonceSize {
		return nil, errors.New("invalid nonce")
	}
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, errors.New("secret authentication failed")
	}
	return plaintext, nil
}

// AAD builds the canonical associated data for a secret row.
func AAD(tenantID, endpointID, secretID string, keyVersion int) []byte {
	return []byte(fmt.Sprintf("%s:%s:%s:%d", tenantID, endpointID, secretID, keyVersion))
}
