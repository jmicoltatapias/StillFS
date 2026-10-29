// Package config loads and validates StillFS process configuration.
package config

import (
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config contains the settings needed to run the initial HTTP process.
type Config struct {
	HTTPAddr                string
	StorageRoot             string
	ReadHeaderTimeout       time.Duration
	ShutdownTimeout         time.Duration
	Database                DatabaseConfig
	CredentialEncryptionKey []byte
	S3Region                string
	S3ClockSkew             time.Duration
	MaxObjectBytes          int64
	MaxHeaderBytes          int
	MaxMetadataBytes        int
}

// DatabaseConfig contains PostgreSQL connection and pool settings.
type DatabaseConfig struct {
	Host            string
	Port            int
	Name            string
	User            string
	Password        string
	SSLMode         string
	SSLRootCert     string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnectTimeout  time.Duration
}

// Load reads configuration from the environment and validates it.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:          envOr("STILLFS_HTTP_ADDR", ":9000"),
		StorageRoot:       envOr("STILLFS_STORAGE_ROOT", "./data/objects"),
		ReadHeaderTimeout: 15 * time.Second,
		ShutdownTimeout:   10 * time.Second,
		S3Region:          envOr("STILLFS_S3_REGION", "us-east-1"),
		S3ClockSkew:       5 * time.Minute,
		MaxObjectBytes:    5 << 30,
		MaxHeaderBytes:    1 << 20,
		MaxMetadataBytes:  8 << 10,
		Database: DatabaseConfig{
			Host:            envOr("STILLFS_DB_HOST", "localhost"),
			Port:            envInt("STILLFS_DB_PORT", 5432),
			Name:            envOr("STILLFS_DB_NAME", "stillfs"),
			User:            envOr("STILLFS_DB_USER", "stillfs"),
			Password:        os.Getenv("STILLFS_DB_PASSWORD"),
			SSLMode:         envOr("STILLFS_DB_SSLMODE", "verify-full"),
			SSLRootCert:     os.Getenv("STILLFS_DB_SSLROOTCERT"),
			MaxOpenConns:    envInt("STILLFS_DB_MAX_OPEN_CONNS", 10),
			MaxIdleConns:    envInt("STILLFS_DB_MAX_IDLE_CONNS", 5),
			ConnMaxLifetime: 5 * time.Minute,
			ConnectTimeout:  5 * time.Second,
		},
	}
	if raw := os.Getenv("STILLFS_CREDENTIAL_ENCRYPTION_KEY"); raw != "" {
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return Config{}, fmt.Errorf("STILLFS_CREDENTIAL_ENCRYPTION_KEY must be base64-encoded")
		}
		cfg.CredentialEncryptionKey = key
	}
	if raw := os.Getenv("STILLFS_S3_CLOCK_SKEW"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid STILLFS_S3_CLOCK_SKEW")
		}
		cfg.S3ClockSkew = parsed
	}
	if raw := os.Getenv("STILLFS_MAX_OBJECT_BYTES"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			cfg.MaxObjectBytes = -1
		} else {
			cfg.MaxObjectBytes = parsed
		}
	}
	if raw := os.Getenv("STILLFS_MAX_HEADER_BYTES"); raw != "" {
		cfg.MaxHeaderBytes = envInt("STILLFS_MAX_HEADER_BYTES", cfg.MaxHeaderBytes)
	}
	if raw := os.Getenv("STILLFS_MAX_METADATA_BYTES"); raw != "" {
		cfg.MaxMetadataBytes = envInt("STILLFS_MAX_METADATA_BYTES", cfg.MaxMetadataBytes)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects settings that would prevent a safe server startup.
func (c Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.HTTPAddr); err != nil {
		return fmt.Errorf("invalid HTTP address %q: %w", c.HTTPAddr, err)
	}
	if strings.TrimSpace(c.StorageRoot) == "" {
		return fmt.Errorf("storage root must not be empty")
	}
	if len(c.CredentialEncryptionKey) != 32 {
		return fmt.Errorf("credential encryption key must decode to exactly 32 bytes")
	}
	if strings.TrimSpace(c.S3Region) == "" || strings.ContainsAny(c.S3Region, "/ \t\r\n") {
		return fmt.Errorf("S3 region is invalid")
	}
	if c.S3ClockSkew <= 0 || c.MaxObjectBytes <= 0 || c.MaxHeaderBytes < 1024 || c.MaxMetadataBytes < 0 || c.MaxMetadataBytes > c.MaxHeaderBytes {
		return fmt.Errorf("S3 request limits or clock skew are invalid")
	}
	if c.ReadHeaderTimeout <= 0 || c.ShutdownTimeout <= 0 {
		return fmt.Errorf("HTTP header and shutdown timeouts must be positive")
	}
	db := c.Database
	if db.Host == "" || db.Name == "" || db.User == "" {
		return fmt.Errorf("database host, name, and user must not be empty")
	}
	if db.Port < 1 || db.Port > 65535 {
		return fmt.Errorf("database port must be between 1 and 65535")
	}
	if db.SSLMode != "disable" && db.SSLMode != "require" && db.SSLMode != "verify-ca" && db.SSLMode != "verify-full" {
		return fmt.Errorf("unsupported database SSL mode %q", db.SSLMode)
	}
	if db.MaxOpenConns < 1 || db.MaxIdleConns < 0 || db.MaxIdleConns > db.MaxOpenConns {
		return fmt.Errorf("database pool limits are invalid")
	}
	if db.ConnMaxLifetime <= 0 || db.ConnectTimeout <= 0 {
		return fmt.Errorf("database connection timeouts must be positive")
	}
	return nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return -1
	}
	return n
}
