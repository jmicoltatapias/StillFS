package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jmicoltatapias/stillfs/internal/credential"
	"github.com/jmicoltatapias/stillfs/internal/storage"
)

func TestValidatePermissionsRejectsAdministrativeAndDuplicateActions(t *testing.T) {
	if err := validatePermissions([]string{"s3:GetObject", "s3:PutObject"}); err != nil {
		t.Fatalf("valid S3 permissions rejected: %v", err)
	}
	for _, permissions := range [][]string{{"admin:Global"}, {"s3:GetObject", "s3:GetObject"}} {
		if err := validatePermissions(permissions); err == nil {
			t.Fatalf("validatePermissions(%v) unexpectedly succeeded", permissions)
		}
	}
}

func TestMasterKeyFailureOutputDoesNotIncludeSecretMaterial(t *testing.T) {
	secret := "master-key-material-must-not-appear"
	var audit auditStub
	commitError := fmt.Errorf("%w: %s", storage.ErrRotationCommitUnknown, secret)
	reason := credential.RotationFailureReason(commitError)
	err := failCredentialOperation(&audit, "credential.master-key-rotation.failed", reason)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe master-key failure error: %v", err)
	}
	if len(audit.events) != 1 || audit.events[0].Action != "credential.master-key-rotation.failed" || audit.events[0].Target != "commit-outcome-unknown" {
		t.Fatalf("failure audit event = %#v", audit.events)
	}
}

type auditStub struct{ events []storage.AuditEvent }

func (a *auditStub) ListAuditEvents(context.Context, int64, int) ([]storage.AuditEvent, error) {
	return a.events, nil
}
func (a *auditStub) AppendAuditEvent(_ context.Context, action, target string) error {
	a.events = append(a.events, storage.AuditEvent{Action: action, Target: target})
	return nil
}
