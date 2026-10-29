// Package postgres owns PostgreSQL connection management and schema migrations.
package postgres

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jmicoltatapias/stillfs/internal/config"
	_ "github.com/lib/pq"
)

//go:embed migrations
var migrationFiles embed.FS

// Open creates and checks a PostgreSQL connection pool.
func Open(ctx context.Context, cfg config.DatabaseConfig) (*sql.DB, error) {
	dsn := fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s connect_timeout=%d",
		quoteDSN(cfg.Host), cfg.Port, quoteDSN(cfg.Name), quoteDSN(cfg.User), quoteDSN(cfg.Password), cfg.SSLMode, int(cfg.ConnectTimeout.Seconds()))
	if cfg.SSLRootCert != "" {
		dsn += " sslrootcert=" + quoteDSN(cfg.SSLRootCert)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL pool: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("check PostgreSQL connection: %w", err)
	}
	return db, nil
}

// HealthCheck verifies PostgreSQL connectivity within the caller's deadline.
func HealthCheck(ctx context.Context, db *sql.DB) error {
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("PostgreSQL health check: %w", err)
	}
	return nil
}

// Checker adapts a connection pool to the HTTP health interface.
type Checker struct{ DB *sql.DB }

// HealthCheck verifies that the pool can reach PostgreSQL.
func (c Checker) HealthCheck(ctx context.Context) error { return HealthCheck(ctx, c.DB) }

// Migrate applies each unapplied embedded SQL migration exactly once.
func Migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS stillfs_schema_migrations (
		version BIGINT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("create migration tracking table: %w", err)
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	type migration struct {
		name    string
		version int64
		body    []byte
	}
	migrations := make([]migration, 0, len(entries))
	versions := make(map[int64]string, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			version, err := migrationVersion(entry.Name())
			if err != nil {
				return err
			}
			if previous, ok := versions[version]; ok {
				return fmt.Errorf("duplicate migration version %d in %q and %q", version, previous, entry.Name())
			}
			body, err := migrationFiles.ReadFile(filepath.Join("migrations", entry.Name()))
			if err != nil {
				return fmt.Errorf("read migration %s: %w", entry.Name(), err)
			}
			versions[version] = entry.Name()
			migrations = append(migrations, migration{name: entry.Name(), version: version, body: body})
		}
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	for _, m := range migrations {
		if err := applyMigration(ctx, db, m.version, m.name, string(m.body)); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, version int64, name, body string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", name, err)
	}
	defer tx.Rollback()
	// The transaction-scoped advisory lock serializes the version check and
	// application when two StillFS processes start against the same database.
	var locked int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM pg_advisory_xact_lock(1398030676)`).Scan(&locked); err != nil {
		return fmt.Errorf("lock migration %s: %w", name, err)
	}
	var applied bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM stillfs_schema_migrations WHERE version = $1)`, version).Scan(&applied); err != nil {
		return fmt.Errorf("check migration %s: %w", name, err)
	}
	if applied {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("finish migration %s: %w", name, err)
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("execute migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO stillfs_schema_migrations(version) VALUES ($1)`, version); err != nil {
		return fmt.Errorf("record migration %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", name, err)
	}
	return nil
}

func migrationVersion(name string) (int64, error) {
	parts := strings.SplitN(name, "_", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid migration filename %q", name)
	}
	version, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("invalid migration version in %q", name)
	}
	return version, nil
}

func quoteDSN(value string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value) + "'"
}
