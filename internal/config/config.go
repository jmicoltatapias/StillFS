// Package config loads and validates StillFS process configuration.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config contains the settings needed to run the initial HTTP process.
type Config struct {
	HTTPAddr        string
	StorageRoot     string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
	Database        DatabaseConfig
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
		HTTPAddr:        envOr("STILLFS_HTTP_ADDR", ":9000"),
		StorageRoot:     envOr("STILLFS_STORAGE_ROOT", "./data/objects"),
		ReadTimeout:     15 * time.Second,
		WriteTimeout:    30 * time.Second,
		ShutdownTimeout: 10 * time.Second,
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
	if c.ReadTimeout <= 0 || c.WriteTimeout <= 0 || c.ShutdownTimeout <= 0 {
		return fmt.Errorf("HTTP and shutdown timeouts must be positive")
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
