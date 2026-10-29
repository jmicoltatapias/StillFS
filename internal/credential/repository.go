package credential

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jmicoltatapias/stillfs/internal/sigv4"
	"github.com/jmicoltatapias/stillfs/internal/storage"
)

// Resolver loads an active credential and decrypts its secret only for the
// duration required by SigV4 verification.
type Resolver struct {
	Store storage.CredentialStore
	Key   []byte
}

func (r Resolver) Resolve(ctx context.Context, accessKeyID string) (sigv4.Credential, error) {
	stored, err := r.Store.GetCredential(ctx, accessKeyID)
	if errors.Is(err, storage.ErrCredentialNotFound) {
		return sigv4.Credential{}, sigv4.ErrUnknownCredential
	}
	if err != nil {
		return sigv4.Credential{}, err
	}
	if len(stored.MasterKeyCheck) != 0 {
		check, checkErr := MasterKeyCheck(r.Key)
		if checkErr != nil || subtle.ConstantTimeCompare(check, stored.MasterKeyCheck) != 1 {
			return sigv4.Credential{}, ErrMasterKeyMismatch
		}
	}
	if stored.RevokedAt != nil {
		return sigv4.Credential{PrincipalID: stored.PrincipalID, Active: false}, nil
	}
	secret, err := Decrypt(r.Key, stored.AccessKeyID, stored.EncryptedSecret, stored.Nonce)
	if err != nil {
		return sigv4.Credential{}, err
	}
	return sigv4.Credential{Secret: secret, PrincipalID: stored.PrincipalID, Active: true}, nil
}

type Manager struct {
	Store storage.CredentialStore
	Key   []byte
}

type Created struct {
	AccessKeyID string
	Secret      string
	PrincipalID int64
}

// Create generates a new key and either creates a principal or adds a key to
// an existing principal. The secret is returned once to the local CLI caller.
func (m Manager) Create(ctx context.Context, principalID int64) (Created, error) {
	var idBytes [20]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return Created{}, fmt.Errorf("generate access key ID: %w", err)
	}
	accessKeyID := "SFS" + hex.EncodeToString(idBytes[:])
	secret, err := GenerateSecret()
	if err != nil {
		return Created{}, err
	}
	ciphertext, nonce, err := Encrypt(m.Key, accessKeyID, secret)
	if err != nil {
		return Created{}, err
	}
	keyCheck, err := MasterKeyCheck(m.Key)
	if err != nil {
		return Created{}, err
	}
	stored, err := m.Store.CreateCredential(ctx, storage.Credential{
		AccessKeyID: accessKeyID, PrincipalID: principalID,
		EncryptedSecret: ciphertext, Nonce: nonce, MasterKeyCheck: keyCheck,
	})
	if err != nil {
		return Created{}, err
	}
	return Created{AccessKeyID: accessKeyID, Secret: secret, PrincipalID: stored.PrincipalID}, nil
}

func (m Manager) Revoke(ctx context.Context, accessKeyID string) error {
	return m.Store.RevokeCredential(ctx, accessKeyID)
}
