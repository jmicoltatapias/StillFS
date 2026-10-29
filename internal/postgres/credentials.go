package postgres

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmicoltatapias/stillfs/internal/storage"
)

// CredentialRepository stores encrypted SigV4 secrets and principal identity.
type CredentialRepository struct{ db *sql.DB }

const credentialManagementAdvisoryLock int64 = 1398030677

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
	if err := lockCredentialManagement(ctx, tx); err != nil {
		return storage.Credential{}, mapDBError(err)
	}
	if len(credential.MasterKeyCheck) != 32 {
		var stateExists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM credential_master_key_state WHERE singleton=1)`).Scan(&stateExists); err != nil {
			return storage.Credential{}, mapDBError(err)
		}
		if stateExists {
			return storage.Credential{}, storage.ErrMasterKeyMismatch
		}
	} else if err := ensureMasterKeyCheck(ctx, tx, credential.MasterKeyCheck); err != nil {
		return storage.Credential{}, err
	}
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events(actor, action, target) VALUES (current_user, 'credential.create', $1)`, credential.AccessKeyID); err != nil {
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
		SELECT c.access_key_id, c.principal_id, c.secret_ciphertext, c.secret_nonce, c.created_at, c.revoked_at, s.key_check
		FROM credentials c LEFT JOIN credential_master_key_state s ON s.singleton=1
		WHERE c.access_key_id = $1`, accessKeyID).Scan(
		&credential.AccessKeyID, &credential.PrincipalID, &credential.EncryptedSecret,
		&credential.Nonce, &credential.CreatedAt, &revoked, &credential.MasterKeyCheck)
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
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return mapDBError(err)
	}
	defer tx.Rollback()
	if err := lockCredentialManagement(ctx, tx); err != nil {
		return mapDBError(err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE credentials SET revoked_at = COALESCE(revoked_at, CURRENT_TIMESTAMP) WHERE access_key_id = $1`, accessKeyID)
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM credential_permissions WHERE access_key_id = $1`, accessKeyID); err != nil {
		return mapDBError(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events(actor, action, target) VALUES (current_user, 'credential.revoke', $1)`, accessKeyID); err != nil {
		return mapDBError(err)
	}
	return mapDBError(tx.Commit())
}

func (r *CredentialRepository) HasCredentialPermission(ctx context.Context, accessKeyID string, principalID int64, permission string) (bool, error) {
	var allowed bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM credential_permissions p JOIN credentials c USING (access_key_id)
		WHERE p.access_key_id=$1 AND c.principal_id=$2 AND p.permission=$3
	)`, accessKeyID, principalID, permission).Scan(&allowed)
	return allowed, mapDBError(err)
}

func (r *CredentialRepository) SetCredentialPermissions(ctx context.Context, accessKeyID string, permissions []string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return mapDBError(err)
	}
	defer tx.Rollback()
	if err := lockCredentialManagement(ctx, tx); err != nil {
		return mapDBError(err)
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM credentials WHERE access_key_id=$1 AND revoked_at IS NULL)`, accessKeyID).Scan(&exists); err != nil {
		return mapDBError(err)
	}
	if !exists {
		return storage.ErrCredentialNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM credential_permissions WHERE access_key_id=$1`, accessKeyID); err != nil {
		return mapDBError(err)
	}
	for _, permission := range permissions {
		if _, err := tx.ExecContext(ctx, `INSERT INTO credential_permissions(access_key_id, permission) VALUES ($1,$2)`, accessKeyID, permission); err != nil {
			return mapDBError(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events(actor, action, target) VALUES (current_user, 'credential.permissions.set', $1)`, accessKeyID); err != nil {
		return mapDBError(err)
	}
	return mapDBError(tx.Commit())
}

func (r *CredentialRepository) ListAuditEvents(ctx context.Context, after int64, limit int) ([]storage.AuditEvent, error) {
	if limit < 1 || limit > 500 {
		return nil, storage.ErrInvalidPageLimit
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, actor, action, target, occurred_at FROM audit_events WHERE id > $1 ORDER BY id LIMIT $2`, after, limit)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer rows.Close()
	events := make([]storage.AuditEvent, 0)
	for rows.Next() {
		var event storage.AuditEvent
		if err := rows.Scan(&event.ID, &event.Actor, &event.Action, &event.Target, &event.OccurredAt); err != nil {
			return nil, mapDBError(err)
		}
		events = append(events, event)
	}
	return events, mapDBError(rows.Err())
}

func (r *CredentialRepository) AppendAuditEvent(ctx context.Context, action, target string) error {
	if action == "" || target == "" {
		return storage.ErrInvalidCredential
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO audit_events(actor, action, target) VALUES (current_user, $1, $2)`, action, target)
	return mapDBError(err)
}

var _ storage.CredentialStore = (*CredentialRepository)(nil)
var _ storage.AuthorizationStore = (*CredentialRepository)(nil)
var _ storage.AuditStore = (*CredentialRepository)(nil)
var _ storage.CredentialSecretRotator = (*CredentialRepository)(nil)
var _ storage.CredentialSecretVerifier = (*CredentialRepository)(nil)

func lockCredentialManagement(ctx context.Context, tx *sql.Tx) error {
	var locked int
	return tx.QueryRowContext(ctx, `SELECT 1 FROM pg_advisory_xact_lock($1)`, credentialManagementAdvisoryLock).Scan(&locked)
}

func ensureMasterKeyCheck(ctx context.Context, tx *sql.Tx, check []byte) error {
	var current []byte
	err := tx.QueryRowContext(ctx, `SELECT key_check FROM credential_master_key_state WHERE singleton=1 FOR UPDATE`).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO credential_master_key_state(singleton, key_check, generation) VALUES (1,$1,1)`, check)
		if err != nil {
			return mapDBError(err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_events(actor,action,target) VALUES(current_user,'credential.master-key-state.initialized','master-key')`)
		return mapDBError(err)
	}
	if err != nil {
		return mapDBError(err)
	}
	if subtle.ConstantTimeCompare(current, check) != 1 {
		return storage.ErrMasterKeyMismatch
	}
	return nil
}

// EnsureCredentialMasterKey initializes the verifier for pre-0004 databases
// or rejects a process configured with a stale/wrong master key.
func (r *CredentialRepository) EnsureCredentialMasterKey(ctx context.Context, check []byte) error {
	if len(check) != 32 {
		return storage.ErrInvalidCredential
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return mapDBError(err)
	}
	defer tx.Rollback()
	if err := lockCredentialManagement(ctx, tx); err != nil {
		return mapDBError(err)
	}
	if err := ensureMasterKeyCheck(ctx, tx, check); err != nil {
		return err
	}
	return mapDBError(tx.Commit())
}

// RotateCredentialSecrets locks all credential-management writes, validates
// the old key verifier, transforms every row, updates the verifier, records
// success, and commits as one PostgreSQL transaction.
func (r *CredentialRepository) RotateCredentialSecrets(ctx context.Context, oldCheck, newCheck []byte, transform func(string, []byte, []byte) ([]byte, []byte, error)) (int, error) {
	if len(oldCheck) != 32 || len(newCheck) != 32 || transform == nil {
		return 0, storage.ErrInvalidCredential
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, mapDBError(err)
	}
	defer tx.Rollback()
	if err := lockCredentialManagement(ctx, tx); err != nil {
		return 0, mapDBError(err)
	}
	var current []byte
	err = tx.QueryRowContext(ctx, `SELECT key_check FROM credential_master_key_state WHERE singleton=1 FOR UPDATE`).Scan(&current)
	stateExists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, mapDBError(err)
	}
	if stateExists && subtle.ConstantTimeCompare(current, oldCheck) != 1 {
		return 0, storage.ErrMasterKeyMismatch
	}
	rows, err := tx.QueryContext(ctx, `SELECT access_key_id, secret_ciphertext, secret_nonce FROM credentials ORDER BY access_key_id FOR UPDATE`)
	if err != nil {
		return 0, mapDBError(err)
	}
	type item struct {
		id                string
		ciphertext, nonce []byte
	}
	items := make([]item, 0)
	for rows.Next() {
		var v item
		if err := rows.Scan(&v.id, &v.ciphertext, &v.nonce); err != nil {
			rows.Close()
			return 0, mapDBError(err)
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, mapDBError(err)
	}
	if err := rows.Close(); err != nil {
		return 0, mapDBError(err)
	}
	for _, item := range items {
		ciphertext, nonce, err := transform(item.id, item.ciphertext, item.nonce)
		if err != nil {
			return 0, err
		}
		if len(ciphertext) < 16 || len(nonce) != 12 {
			return 0, storage.ErrInvalidCredential
		}
		if _, err := tx.ExecContext(ctx, `UPDATE credentials SET secret_ciphertext=$2, secret_nonce=$3 WHERE access_key_id=$1`, item.id, ciphertext, nonce); err != nil {
			return 0, mapDBError(err)
		}
	}
	if stateExists {
		if _, err := tx.ExecContext(ctx, `UPDATE credential_master_key_state SET key_check=$1, generation=generation+1, updated_at=CURRENT_TIMESTAMP WHERE singleton=1`, newCheck); err != nil {
			return 0, mapDBError(err)
		}
	} else if _, err := tx.ExecContext(ctx, `INSERT INTO credential_master_key_state(singleton,key_check,generation) VALUES (1,$1,1)`, newCheck); err != nil {
		return 0, mapDBError(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events(actor,action,target) VALUES(current_user,'credential.master-key-rotation.succeeded',$1)`, fmt.Sprintf("credential-count:%d", len(items))); err != nil {
		return 0, mapDBError(err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("%w: %v", storage.ErrRotationCommitUnknown, err)
	}
	return len(items), nil
}

func (r *CredentialRepository) VerifyCredentialSecrets(ctx context.Context, check []byte, verify func(string, []byte, []byte) error) (int, error) {
	if len(check) != 32 || verify == nil {
		return 0, storage.ErrInvalidCredential
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, mapDBError(err)
	}
	defer tx.Rollback()
	if err := lockCredentialManagement(ctx, tx); err != nil {
		return 0, mapDBError(err)
	}
	var current []byte
	err = tx.QueryRowContext(ctx, `SELECT key_check FROM credential_master_key_state WHERE singleton=1 FOR UPDATE`).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, mapDBError(err)
	}
	if err == nil && subtle.ConstantTimeCompare(current, check) != 1 {
		return 0, storage.ErrMasterKeyMismatch
	}
	rows, err := tx.QueryContext(ctx, `SELECT access_key_id,secret_ciphertext,secret_nonce FROM credentials ORDER BY access_key_id FOR UPDATE`)
	if err != nil {
		return 0, mapDBError(err)
	}
	type item struct {
		id                string
		ciphertext, nonce []byte
	}
	items := make([]item, 0)
	for rows.Next() {
		var v item
		if err := rows.Scan(&v.id, &v.ciphertext, &v.nonce); err != nil {
			rows.Close()
			return 0, mapDBError(err)
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, mapDBError(err)
	}
	if err := rows.Close(); err != nil {
		return 0, mapDBError(err)
	}
	for _, item := range items {
		if err := verify(item.id, item.ciphertext, item.nonce); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, mapDBError(err)
	}
	return len(items), nil
}
