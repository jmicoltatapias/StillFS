// Package credential encrypts SigV4 secrets and provides local credential
// provisioning helpers. It does not implement authorization policies.
package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
)

var keyCheckContext = []byte("StillFS credential master-key check v1")

// MasterKeyCheck returns a keyed verifier used to ensure all StillFS processes
// and credential writers use the active encryption key. It is not reversible.
func MasterKeyCheck(masterKey []byte) ([]byte, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("credential encryption key must contain 32 bytes")
	}
	mac := hmac.New(sha256.New, masterKey)
	_, _ = mac.Write(keyCheckContext)
	return mac.Sum(nil), nil
}

// Encrypt protects a secret at rest using AES-256-GCM and binds its ciphertext
// to the access key ID as associated data.
func Encrypt(masterKey []byte, accessKeyID, secret string) (ciphertext, nonce []byte, err error) {
	return encryptBytes(masterKey, accessKeyID, []byte(secret))
}

func encryptBytes(masterKey []byte, accessKeyID string, secret []byte) (ciphertext, nonce []byte, err error) {
	aead, err := newAEAD(masterKey)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate credential nonce: %w", err)
	}
	ciphertext = aead.Seal(nil, nonce, secret, []byte(accessKeyID))
	return ciphertext, nonce, nil
}

// Decrypt opens a stored SigV4 secret. Plaintext must only be retained for the
// duration of request verification and must never be logged or returned.
func Decrypt(masterKey []byte, accessKeyID string, ciphertext, nonce []byte) (string, error) {
	plaintext, err := decryptBytes(masterKey, accessKeyID, ciphertext, nonce)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func decryptBytes(masterKey []byte, accessKeyID string, ciphertext, nonce []byte) ([]byte, error) {
	aead, err := newAEAD(masterKey)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("invalid credential nonce")
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, []byte(accessKeyID))
	if err != nil {
		return nil, fmt.Errorf("decrypt credential secret: %w", err)
	}
	return plaintext, nil
}

func newAEAD(masterKey []byte) (cipher.AEAD, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("credential encryption key must contain 32 bytes")
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("initialize credential encryption: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize credential encryption: %w", err)
	}
	return aead, nil
}

// GenerateSecret creates a high-entropy secret suitable for a SigV4 key.
func GenerateSecret() (string, error) {
	bytes := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, bytes); err != nil {
		return "", fmt.Errorf("generate credential secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
