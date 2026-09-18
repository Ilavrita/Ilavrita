package project

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

var (
	// ErrMissingSealingKey reports that nothing was configured to seal with. It
	// is refused rather than defaulted: a key this server chose for itself would
	// be one an attacker can choose too.
	ErrMissingSealingKey = errors.New("project: no key is configured to seal secrets with")

	// ErrMalformedSealingKey reports a key that is not 32 random bytes.
	ErrMalformedSealingKey = errors.New("project: a sealing key is 32 bytes, base64-encoded")

	// ErrUnsealable reports material this key cannot open: a different key
	// sealed it, or something changed it since.
	ErrUnsealable = errors.New("project: the sealed material cannot be opened")
)

// sealingKeyBytes is AES-256's key length.
const sealingKeyBytes = 32

// SealingKey encrypts the secrets this server has to keep rather than hash.
//
// A password can be hashed because the server only ever has to recognise it. A
// second-factor secret cannot: the server computes the same code the phone
// does, so whatever holds the secret holds the factor. Sealing it means a
// database read on its own — a leaked backup, a replica, a stolen file — does
// not hand over anyone's second factor, because the key lives in the
// deployment's environment rather than in the file.
//
// It does not defend against an attacker who has both. Nothing short of a
// hardware module does.
type SealingKey struct {
	block cipher.AEAD
}

// ParseSealingKey reads the configured key.
func ParseSealingKey(encoded string) (SealingKey, error) {
	if encoded == "" {
		return SealingKey{}, ErrMissingSealingKey
	}

	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return SealingKey{}, fmt.Errorf("%w: %w", ErrMalformedSealingKey, err)
	}

	if len(raw) != sealingKeyBytes {
		return SealingKey{}, fmt.Errorf("%w: %d bytes", ErrMalformedSealingKey, len(raw))
	}

	block, err := aes.NewCipher(raw)
	if err != nil {
		return SealingKey{}, fmt.Errorf("%w: %w", ErrMalformedSealingKey, err)
	}

	sealed, err := cipher.NewGCM(block)
	if err != nil {
		return SealingKey{}, fmt.Errorf("%w: %w", ErrMalformedSealingKey, err)
	}

	return SealingKey{block: sealed}, nil
}

// IsZero reports whether this is a key nobody configured.
func (k SealingKey) IsZero() bool { return k.block == nil }

// Seal encrypts material for storage. The nonce is drawn per call and carried
// in front of the ciphertext, so sealing the same secret twice never produces
// the same row.
func (k SealingKey) Seal(plaintext []byte) (string, error) {
	if k.IsZero() {
		return "", ErrMissingSealingKey
	}

	nonce := make([]byte, k.block.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("project: cannot draw a nonce: %w", err)
	}

	return base64.StdEncoding.EncodeToString(k.block.Seal(nonce, nonce, plaintext, nil)), nil
}

// Open decrypts what Seal wrote. GCM authenticates as it decrypts, so material
// a different key sealed, or that anything has changed since, fails here rather
// than being read as something else.
func (k SealingKey) Open(sealed string) ([]byte, error) {
	if k.IsZero() {
		return nil, ErrMissingSealingKey
	}

	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsealable, err)
	}

	if len(raw) < k.block.NonceSize() {
		return nil, fmt.Errorf("%w: %d bytes", ErrUnsealable, len(raw))
	}

	nonce, body := raw[:k.block.NonceSize()], raw[k.block.NonceSize():]

	plaintext, err := k.block.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsealable, err)
	}

	return plaintext, nil
}

// MintSealingKey draws one, for an operator setting a deployment up.
func MintSealingKey(random io.Reader) (string, error) {
	raw := make([]byte, sealingKeyBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", fmt.Errorf("project: cannot mint a sealing key: %w", err)
	}

	return base64.StdEncoding.EncodeToString(raw), nil
}
