package credential

import (
	"context"
	"crypto/subtle"
	"errors"

	"github.com/jmicoltatapias/stillfs/internal/storage"
)

var (
	ErrInvalidMasterKey      = errors.New("master key must be exactly 32 bytes")
	ErrMasterKeyMismatch     = storage.ErrMasterKeyMismatch
	ErrInvalidCredentialData = errors.New("stored credential secret cannot be verified")
	ErrSameMasterKey         = errors.New("new master key must differ from the current key")
)

// RotateMasterKey records the attempt and re-encrypts every credential through
// one repository transaction. Plaintext exists only transiently in process
// memory and is never written to disk or logged.
func RotateMasterKey(ctx context.Context, store storage.CredentialSecretRotator, oldKey, newKey []byte) (int, error) {
	if len(oldKey) != 32 || len(newKey) != 32 {
		return 0, ErrInvalidMasterKey
	}
	if subtle.ConstantTimeCompare(oldKey, newKey) == 1 {
		return 0, ErrSameMasterKey
	}
	oldCheck, _ := MasterKeyCheck(oldKey)
	newCheck, _ := MasterKeyCheck(newKey)
	count, err := store.RotateCredentialSecrets(ctx, oldCheck, newCheck, func(accessKeyID string, ciphertext, nonce []byte) ([]byte, []byte, error) {
		plain, err := decryptBytes(oldKey, accessKeyID, ciphertext, nonce)
		if err != nil {
			return nil, nil, ErrInvalidCredentialData
		}
		defer wipe(plain)
		newCiphertext, newNonce, err := encryptBytes(newKey, accessKeyID, plain)
		if err != nil {
			return nil, nil, ErrInvalidCredentialData
		}
		verified, err := decryptBytes(newKey, accessKeyID, newCiphertext, newNonce)
		if err != nil {
			return nil, nil, ErrInvalidCredentialData
		}
		defer wipe(verified)
		if subtle.ConstantTimeCompare(plain, verified) != 1 {
			return nil, nil, ErrInvalidCredentialData
		}
		return newCiphertext, newNonce, nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// VerifyMasterKey validates a candidate key against PostgreSQL and all stored
// credential ciphertext without changing application records.
func VerifyMasterKey(ctx context.Context, store storage.CredentialSecretVerifier, key []byte) (int, error) {
	if len(key) != 32 {
		return 0, ErrInvalidMasterKey
	}
	check, _ := MasterKeyCheck(key)
	return store.VerifyCredentialSecrets(ctx, check, func(accessKeyID string, ciphertext, nonce []byte) error {
		plain, err := decryptBytes(key, accessKeyID, ciphertext, nonce)
		if err != nil {
			return ErrInvalidCredentialData
		}
		wipe(plain)
		return nil
	})
}

// RotationFailureReason maps errors to a bounded, non-sensitive audit value.
func RotationFailureReason(err error) string {
	switch {
	case errors.Is(err, ErrInvalidMasterKey):
		return "invalid-key-length"
	case errors.Is(err, ErrSameMasterKey):
		return "same-key"
	case errors.Is(err, ErrMasterKeyMismatch):
		return "old-key-mismatch"
	case errors.Is(err, ErrInvalidCredentialData):
		return "credential-data-invalid"
	case errors.Is(err, storage.ErrRotationCommitUnknown):
		return "commit-outcome-unknown"
	default:
		return "database-operation"
	}
}

func wipe(secret []byte) {
	for i := range secret {
		secret[i] = 0
	}
}
