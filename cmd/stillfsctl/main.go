package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jmicoltatapias/stillfs/internal/config"
	"github.com/jmicoltatapias/stillfs/internal/credential"
	"github.com/jmicoltatapias/stillfs/internal/postgres"
	"github.com/jmicoltatapias/stillfs/internal/recovery"
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
	if len(args) < 2 || args[0] != "credential" {
		return errors.New("usage: stillfsctl credential create [--principal-id ID] | credential revoke ACCESS_KEY_ID | storage audit")
	}
	cfg, err := config.Load()
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
		_, err = fmt.Fprintf(stdout, "Principal ID: %d\nAccess key ID: %s\nSecret access key (shown once): %s\n", created.PrincipalID, created.AccessKeyID, created.Secret)
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

func runStorageAudit(stdout io.Writer) error {
	cfg, err := config.Load()
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
