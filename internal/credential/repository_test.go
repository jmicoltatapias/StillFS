package credential

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jmicoltatapias/stillfs/internal/sigv4"
	"github.com/jmicoltatapias/stillfs/internal/storage"
)

func TestManagerCreateResolveAndRevoke(t *testing.T) {
	store := &credentialStoreStub{records: make(map[string]storage.Credential)}
	key := make([]byte, 32)
	manager := Manager{Store: store, Key: key}
	created, err := manager.Create(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if created.PrincipalID <= 0 || created.AccessKeyID == "" || created.Secret == "" {
		t.Fatalf("incomplete created credential: %#v", created)
	}
	stored := store.records[created.AccessKeyID]
	if len(stored.EncryptedSecret) == 0 || string(stored.EncryptedSecret) == created.Secret {
		t.Fatal("credential secret was not encrypted before persistence")
	}
	resolver := Resolver{Store: store, Key: key}
	resolved, err := resolver.Resolve(context.Background(), created.AccessKeyID)
	if err != nil || !resolved.Active || resolved.PrincipalID != created.PrincipalID || resolved.Secret != created.Secret {
		t.Fatalf("Resolve() = (%#v, %v)", resolved, err)
	}
	if err := manager.Revoke(context.Background(), created.AccessKeyID); err != nil {
		t.Fatal(err)
	}
	resolved, err = resolver.Resolve(context.Background(), created.AccessKeyID)
	if err != nil || resolved.Active || resolved.Secret != "" {
		t.Fatalf("revoked Resolve() = (%#v, %v)", resolved, err)
	}
	if _, err := resolver.Resolve(context.Background(), "unknown"); !errors.Is(err, sigv4.ErrUnknownCredential) {
		t.Fatalf("unknown credential error = %v", err)
	}
}

type credentialStoreStub struct{ records map[string]storage.Credential }

func (s *credentialStoreStub) CreateCredential(_ context.Context, item storage.Credential) (storage.Credential, error) {
	if _, ok := s.records[item.AccessKeyID]; ok {
		return storage.Credential{}, storage.ErrAlreadyExists
	}
	if item.PrincipalID == 0 {
		item.PrincipalID = 23
	}
	item.CreatedAt = time.Now()
	s.records[item.AccessKeyID] = item
	return item, nil
}

func (s *credentialStoreStub) GetCredential(_ context.Context, accessKeyID string) (storage.Credential, error) {
	item, ok := s.records[accessKeyID]
	if !ok {
		return storage.Credential{}, storage.ErrCredentialNotFound
	}
	return item, nil
}

func (s *credentialStoreStub) RevokeCredential(_ context.Context, accessKeyID string) error {
	item, ok := s.records[accessKeyID]
	if !ok {
		return storage.ErrCredentialNotFound
	}
	now := time.Now()
	item.RevokedAt = &now
	s.records[accessKeyID] = item
	return nil
}
