package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jmicoltatapias/stillfs/internal/storage"
)

// CredentialRepository stores encrypted SigV4 secrets and principal identity.
type CredentialRepository struct{ db *sql.DB }

func NewCredentialRepository(db *sql.DB) *CredentialRepository {
	return &CredentialRepository{db: db}
}

// CreateCredential creates a principal when PrincipalID is zero, otherwise it
// attaches the credential to the requested existing principal. The secret
// ciphertext is stored in the same transaction as a newly-created principal.
func (r *CredentialRepository) CreateCredential(ctx context.Context, credential storage.Credential) (storage.Credential, error) {
	if credential.AccessKeyID == "" || len(credential.EncryptedSecret) == 0 || len(credential.Nonce) != 12 || credential.PrincipalID < 0 {
		return storage.Credential{}, storage.ErrInvalidCredential
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return storage.Credential{}, mapDBError(err)
	}
	defer tx.Rollback()
	if credential.PrincipalID == 0 {
		if err := tx.QueryRowContext(ctx, `INSERT INTO principals DEFAULT VALUES RETURNING id`).Scan(&credential.PrincipalID); err != nil {
			return storage.Credential{}, mapDBError(err)
		}
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO credentials(access_key_id, principal_id, secret_ciphertext, secret_nonce)
		VALUES ($1, $2, $3, $4)
		RETURNING created_at`, credential.AccessKeyID, credential.PrincipalID,
		credential.EncryptedSecret, credential.Nonce).Scan(&credential.CreatedAt)
	if err != nil {
		return storage.Credential{}, mapDBError(err)
	}
	if err := tx.Commit(); err != nil {
		return storage.Credential{}, mapDBError(err)
	}
	return credential, nil
}

func (r *CredentialRepository) GetCredential(ctx context.Context, accessKeyID string) (storage.Credential, error) {
	var credential storage.Credential
	var revoked sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT access_key_id, principal_id, secret_ciphertext, secret_nonce, created_at, revoked_at
		FROM credentials WHERE access_key_id = $1`, accessKeyID).Scan(
		&credential.AccessKeyID, &credential.PrincipalID, &credential.EncryptedSecret,
		&credential.Nonce, &credential.CreatedAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.Credential{}, storage.ErrCredentialNotFound
	}
	if err != nil {
		return storage.Credential{}, mapDBError(err)
	}
	if revoked.Valid {
		credential.RevokedAt = &revoked.Time
	}
	return credential, nil
}

func (r *CredentialRepository) RevokeCredential(ctx context.Context, accessKeyID string) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE credentials SET revoked_at = COALESCE(revoked_at, CURRENT_TIMESTAMP)
		WHERE access_key_id = $1`, accessKeyID)
	if err != nil {
		return mapDBError(err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return mapDBError(err)
	}
	if updated == 0 {
		return storage.ErrCredentialNotFound
	}
	return nil
}

var _ storage.CredentialStore = (*CredentialRepository)(nil)
