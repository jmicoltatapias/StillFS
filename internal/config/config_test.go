package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func validConfig() Config {
	return Config{
		HTTPAddr:                ":9000",
		StorageRoot:             "./data/objects",
		ReadHeaderTimeout:       time.Second,
		ShutdownTimeout:         time.Second,
		CredentialEncryptionKey: bytes.Repeat([]byte{1}, 32),
		S3Region:                "us-east-1", S3ClockSkew: time.Minute,
		MaxObjectBytes: 1024, MaxHeaderBytes: 1 << 20, MaxMetadataBytes: 8 << 10,
		Database: DatabaseConfig{
			Host: "localhost", Port: 5432, Name: "stillfs", User: "stillfs",
			SSLMode: "disable", MaxOpenConns: 10, MaxIdleConns: 5,
			ConnMaxLifetime: time.Minute, ConnectTimeout: time.Second,
		},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"valid", validConfig(), false},
		{"invalid address", func() Config { c := validConfig(); c.HTTPAddr = "no-port"; return c }(), true},
		{"non-positive timeout", func() Config { c := validConfig(); c.ReadHeaderTimeout = 0; return c }(), true},
		{"invalid database pool", func() Config { c := validConfig(); c.Database.MaxIdleConns = 11; return c }(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadReadsDatabaseEnvironment(t *testing.T) {
	t.Setenv("STILLFS_CREDENTIAL_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("STILLFS_HTTP_ADDR", "127.0.0.1:9001")
	t.Setenv("STILLFS_DB_HOST", "db.internal")
	t.Setenv("STILLFS_DB_PORT", "6432")
	t.Setenv("STILLFS_DB_PASSWORD", "test-secret")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9001" || cfg.Database.Host != "db.internal" || cfg.Database.Port != 6432 || cfg.Database.Password != "test-secret" {
		t.Fatalf("Load() did not read configured values: %#v", cfg)
	}
	if cfg.Database.SSLMode != "verify-full" {
		t.Fatalf("default database SSL mode = %q, want verify-full", cfg.Database.SSLMode)
	}
	if cfg.StorageRoot != "./data/objects" {
		t.Fatalf("default storage root = %q", cfg.StorageRoot)
	}
	if cfg.S3Region != "us-east-1" || len(cfg.CredentialEncryptionKey) != 32 {
		t.Fatalf("S3 configuration not loaded: region=%q encryption key length=%d", cfg.S3Region, len(cfg.CredentialEncryptionKey))
	}
}

func TestLoadRejectsInvalidDatabasePort(t *testing.T) {
	t.Setenv("STILLFS_DB_PORT", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded with an invalid database port")
	}
}

func TestLoadRequiresCredentialEncryptionKey(t *testing.T) {
	t.Setenv("STILLFS_CREDENTIAL_ENCRYPTION_KEY", "")
	t.Setenv("STILLFS_CREDENTIAL_ENCRYPTION_KEY_FILE", "")
	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded without the credential encryption key")
	}
}

func TestLoadReadsProtectedMasterKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "active.key")
	want := bytes.Repeat([]byte{0x31}, 32)
	if err := os.WriteFile(path, want, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STILLFS_CREDENTIAL_ENCRYPTION_KEY", "")
	t.Setenv("STILLFS_CREDENTIAL_ENCRYPTION_KEY_FILE", path)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cfg.CredentialEncryptionKey, want) {
		t.Fatal("loaded key file differs")
	}
}

func TestLoadForCredentialRotationDoesNotRequireOrInspectMasterKey(t *testing.T) {
	t.Setenv("STILLFS_CREDENTIAL_ENCRYPTION_KEY", "not base64")
	t.Setenv("STILLFS_CREDENTIAL_ENCRYPTION_KEY_FILE", "/does/not/exist")
	cfg, err := LoadForCredentialRotation()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CredentialEncryptionKey) != 0 {
		t.Fatal("rotation config unexpectedly loaded a master key")
	}
}
