// Package authorization separates permission decisions from SigV4 identity
// verification. Permissions are fixed StillFS capabilities, not IAM policies.
package authorization

import (
	"context"
	"errors"

	"github.com/jmicoltatapias/stillfs/internal/sigv4"
	"github.com/jmicoltatapias/stillfs/internal/storage"
)

const (
	ListBuckets  = "s3:ListBuckets"
	ListObjects  = "s3:ListObjects"
	CreateBucket = "s3:CreateBucket"
	DeleteBucket = "s3:DeleteBucket"
	GetObject    = "s3:GetObject"
	PutObject    = "s3:PutObject"
	DeleteObject = "s3:DeleteObject"
)

var ErrDenied = errors.New("permission denied")

type Authorizer struct{ Store storage.AuthorizationStore }

func (a Authorizer) Authorize(ctx context.Context, identity sigv4.Authenticated, permission string) error {
	if a.Store == nil || identity.AccessKeyID == "" || identity.PrincipalID <= 0 || !knownPermission(permission) {
		return ErrDenied
	}
	allowed, err := a.Store.HasCredentialPermission(ctx, identity.AccessKeyID, identity.PrincipalID, permission)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrDenied
	}
	return nil
}

func KnownPermission(permission string) bool { return knownPermission(permission) }

func knownPermission(permission string) bool {
	switch permission {
	case ListBuckets, ListObjects, CreateBucket, DeleteBucket, GetObject, PutObject, DeleteObject:
		return true
	default:
		return false
	}
}
