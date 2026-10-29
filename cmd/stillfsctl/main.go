package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jmicoltatapias/stillfs/internal/authorization"
	"github.com/jmicoltatapias/stillfs/internal/config"
	"github.com/jmicoltatapias/stillfs/internal/credential"
	"github.com/jmicoltatapias/stillfs/internal/postgres"
	"github.com/jmicoltatapias/stillfs/internal/recovery"
	"github.com/jmicoltatapias/stillfs/internal/securefile"
	"github.com/jmicoltatapias/stillfs/internal/storage"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 2 && args[0] == "storage" && args[1] == "audit" {
		return runStorageAudit(stdout)
	}
	if len(args) > 1 && args[0] == "audit" && args[1] == "list" {
		return runAuditList(args[2:], stdout, stderr)
	}
	if len(args) > 1 && args[0] == "credential" && args[1] == "rotate-master-key" {
		return runRotateMasterKey(args[2:], stdout, stderr)
	}
	if len(args) > 1 && args[0] == "credential" && args[1] == "verify-master-key" {
		return runVerifyMasterKey(args[2:], stdout, stderr)
	}
	if len(args) < 2 || args[0] != "credential" {
		return errors.New("usage: stillfsctl credential create [--principal-id ID] [--permission ACTION ...] | credential permissions set ACCESS_KEY_ID [--permission ACTION ...] | credential revoke ACCESS_KEY_ID | credential rotate-master-key --old-key-file PATH --new-key-file PATH | credential verify-master-key --key-file PATH | audit list | storage audit")
	}
	var cfg config.Config
	var err error
	if args[1] == "create" {
		cfg, err = config.Load()
	} else {
		cfg, err = config.LoadForCredentialRotation()
	}
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := postgres.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := postgres.Migrate(ctx, db); err != nil {
		return err
	}
	manager := credential.Manager{Store: postgres.NewCredentialRepository(db), Key: cfg.CredentialEncryptionKey}
	switch args[1] {
	case "create":
		flags := flag.NewFlagSet("credential create", flag.ContinueOnError)
		flags.SetOutput(stderr)
		principalID := flags.Int64("principal-id", 0, "attach to an existing principal; omit to create a new principal")
		var permissions stringFlags
		flags.Var(&permissions, "permission", "grant one fixed s3:* permission (repeatable); no permission is granted by default")
		if err := flags.Parse(args[2:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *principalID < 0 {
			return errors.New("credential create accepts only --principal-id with a non-negative ID")
		}
		created, err := manager.Create(ctx, *principalID)
		if err != nil {
			return err
		}
		if err := validatePermissions(permissions); err != nil {
			_ = manager.Revoke(ctx, created.AccessKeyID)
			return err
		}
		if len(permissions) > 0 {
			if err := postgres.NewCredentialRepository(db).SetCredentialPermissions(ctx, created.AccessKeyID, permissions); err != nil {
				_ = manager.Revoke(ctx, created.AccessKeyID)
				return err
			}
		}
		_, err = fmt.Fprintf(stdout, "Principal ID: %d\nAccess key ID: %s\nSecret access key (shown once): %s\n", created.PrincipalID, created.AccessKeyID, created.Secret)
		return err
	case "permissions":
		if len(args) < 3 || args[2] != "set" || len(args) < 4 {
			return errors.New("usage: stillfsctl credential permissions set ACCESS_KEY_ID [--permission ACTION ...]")
		}
		flags := flag.NewFlagSet("credential permissions set", flag.ContinueOnError)
		flags.SetOutput(stderr)
		var permissions stringFlags
		flags.Var(&permissions, "permission", "fixed s3:* permission to grant (repeatable; omit all to revoke all permissions)")
		if err := flags.Parse(args[4:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected credential permissions arguments")
		}
		if err := validatePermissions(permissions); err != nil {
			return err
		}
		if err := postgres.NewCredentialRepository(db).SetCredentialPermissions(ctx, args[3], permissions); err != nil {
			return err
		}
		_, err := fmt.Fprintln(stdout, "credential permissions updated")
		return err
	case "revoke":
		if len(args) != 3 || args[2] == "" {
			return errors.New("usage: stillfsctl credential revoke ACCESS_KEY_ID")
		}
		if err := manager.Revoke(ctx, args[2]); err != nil {
			return err
		}
		_, err := fmt.Fprintln(stdout, "credential revoked")
		return err
	default:
		return errors.New("unknown credential command")
	}
}

type stringFlags []string

func (s *stringFlags) String() string         { return fmt.Sprint([]string(*s)) }
func (s *stringFlags) Set(value string) error { *s = append(*s, value); return nil }

func validatePermissions(permissions []string) error {
	seen := make(map[string]bool)
	for _, permission := range permissions {
		if !authorization.KnownPermission(permission) {
			return fmt.Errorf("unsupported permission %q", permission)
		}
		if seen[permission] {
			return fmt.Errorf("duplicate permission %q", permission)
		}
		seen[permission] = true
	}
	return nil
}

func runRotateMasterKey(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("credential rotate-master-key", flag.ContinueOnError)
	flags.SetOutput(stderr)
	oldPath := flags.String("old-key-file", "", "protected file containing the current raw 32-byte key")
	newPath := flags.String("new-key-file", "", "protected file containing the replacement raw 32-byte key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *oldPath == "" || *newPath == "" {
		return errors.New("usage: stillfsctl credential rotate-master-key --old-key-file PATH --new-key-file PATH")
	}
	db, ctx, cancel, err := openCredentialAdminDB()
	if err != nil {
		return err
	}
	defer cancel()
	defer db.Close()
	repository := postgres.NewCredentialRepository(db)
	if err := repository.AppendAuditEvent(ctx, "credential.master-key-rotation.started", "all-credentials"); err != nil {
		return err
	}
	oldKey, err := securefile.ReadMasterKey(*oldPath)
	if err != nil {
		return failCredentialOperation(repository, "credential.master-key-rotation.failed", "invalid-old-key-file")
	}
	defer wipeCLIKey(oldKey)
	newKey, err := securefile.ReadMasterKey(*newPath)
	if err != nil {
		return failCredentialOperation(repository, "credential.master-key-rotation.failed", "invalid-new-key-file")
	}
	defer wipeCLIKey(newKey)
	count, err := credential.RotateMasterKey(ctx, repository, oldKey, newKey)
	if err != nil {
		return failCredentialOperation(repository, "credential.master-key-rotation.failed", credential.RotationFailureReason(err))
	}
	_, err = fmt.Fprintf(stdout, "credential encryption key rotated for %d credentials\n", count)
	return err
}

func runVerifyMasterKey(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("credential verify-master-key", flag.ContinueOnError)
	flags.SetOutput(stderr)
	keyPath := flags.String("key-file", "", "protected file containing a raw 32-byte candidate key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *keyPath == "" {
		return errors.New("usage: stillfsctl credential verify-master-key --key-file PATH")
	}
	db, ctx, cancel, err := openCredentialAdminDB()
	if err != nil {
		return err
	}
	defer cancel()
	defer db.Close()
	repository := postgres.NewCredentialRepository(db)
	if err := repository.AppendAuditEvent(ctx, "credential.master-key-verification.started", "all-credentials"); err != nil {
		return err
	}
	key, err := securefile.ReadMasterKey(*keyPath)
	if err != nil {
		return failCredentialOperation(repository, "credential.master-key-verification.failed", "invalid-key-file")
	}
	defer wipeCLIKey(key)
	count, err := credential.VerifyMasterKey(ctx, repository, key)
	if err != nil {
		return failCredentialOperation(repository, "credential.master-key-verification.failed", credential.RotationFailureReason(err))
	}
	if err := repository.AppendAuditEvent(ctx, "credential.master-key-verification.succeeded", fmt.Sprintf("credential-count:%d", count)); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "master key verified for %d credentials\n", count)
	return err
}

func openCredentialAdminDB() (*sql.DB, context.Context, func(), error) {
	cfg, err := config.LoadForCredentialRotation()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("configuration: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	db, err := postgres.Open(ctx, cfg.Database)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	if err := postgres.Migrate(ctx, db); err != nil {
		db.Close()
		cancel()
		return nil, nil, nil, err
	}
	return db, ctx, cancel, nil
}

func failCredentialOperation(audit storage.AuditStore, action, reason string) error {
	auditCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := audit.AppendAuditEvent(auditCtx, action, reason); err != nil {
		return fmt.Errorf("master-key operation failed (%s); its failure audit event could not be written", reason)
	}
	return fmt.Errorf("master-key operation failed (%s)", reason)
}

func wipeCLIKey(key []byte) {
	for i := range key {
		key[i] = 0
	}
}

func runAuditList(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("audit list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	after := flags.Int64("after", 0, "return events with larger IDs")
	limit := flags.Int("limit", 100, "maximum events (1..500)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *after < 0 {
		return errors.New("usage: stillfsctl audit list [--after ID] [--limit 100]")
	}
	cfg, err := config.LoadForCredentialRotation()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := postgres.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := postgres.Migrate(ctx, db); err != nil {
		return err
	}
	audit := postgres.NewCredentialRepository(db)
	if err := audit.AppendAuditEvent(ctx, "audit.read", "audit_events"); err != nil {
		return err
	}
	events, err := audit.ListAuditEvents(ctx, *after, *limit)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(events)
}

func runStorageAudit(stdout io.Writer) error {
	cfg, err := config.LoadForCredentialRotation()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := postgres.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	metadata := postgres.NewMetadataRepository(db)
	report, err := recovery.Audit(ctx, metadata, cfg.StorageRoot)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}
