package authorization

import (
	"context"
	"errors"
	"testing"

	"github.com/jmicoltatapias/stillfs/internal/sigv4"
)

func TestAuthorizerDeniesMissingIdentityUnknownPermissionAndUnassignedGrant(t *testing.T) {
	store := permissionStore{principals: map[string]int64{"key-a": 7}, grants: map[string]map[string]bool{"key-a": {GetObject: true}}}
	a := Authorizer{Store: store}
	if err := a.Authorize(context.Background(), sigv4.Authenticated{AccessKeyID: "key-a", PrincipalID: 7}, GetObject); err != nil {
		t.Fatalf("assigned action denied: %v", err)
	}
	for _, test := range []struct {
		id         string
		principal  int64
		permission string
	}{{"key-a", 7, PutObject}, {"key-a", 7, "admin:Global"}, {"", 7, GetObject}, {"key-a", 8, GetObject}, {"key-a", 0, GetObject}} {
		if err := a.Authorize(context.Background(), sigv4.Authenticated{AccessKeyID: test.id, PrincipalID: test.principal}, test.permission); !errors.Is(err, ErrDenied) {
			t.Fatalf("Authorize(%q, %q) = %v, want denial", test.id, test.permission, err)
		}
	}
}

type permissionStore struct {
	grants     map[string]map[string]bool
	principals map[string]int64
}

func (s permissionStore) HasCredentialPermission(_ context.Context, key string, principalID int64, permission string) (bool, error) {
	return s.principals[key] == principalID && s.grants[key][permission], nil
}

func (permissionStore) SetCredentialPermissions(context.Context, string, []string) error { return nil }
