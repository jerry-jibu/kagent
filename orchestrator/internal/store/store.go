// Package store owns the orchestrator's PostgreSQL persistence: schema
// migrations, the single-instance advisory lock, and transactional state.
//
// The orchestrator uses its own PostgreSQL schema (docs/designs/0001 D5),
// shared cluster instance, isolated namespace. Connections pin search_path
// to that schema so callers never qualify table names.
package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrInstanceLocked is returned when another orchestrator instance already
// holds the instance lock (docs/designs/0001 §9: P0 is single-replica).
var ErrInstanceLocked = errors.New("instance lock held by another orchestrator")

// Config configures a Store connection.
type Config struct {
	// DatabaseURL is the PostgreSQL connection string.
	DatabaseURL string
	// Schema is the orchestrator-owned PostgreSQL schema. Created on Open.
	// Defaults to "orchestrator".
	Schema string
}

// Store is the orchestrator's persistence facade.
type Store struct {
	pool   *pgxpool.Pool
	schema string
	dbURL  string

	lockMu   sync.Mutex
	lockConn *pgx.Conn
}

// Open connects to PostgreSQL and ensures the orchestrator schema exists.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	schema := cfg.Schema
	if schema == "" {
		schema = "orchestrator"
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse database URL: %w", err)
	}
	// Pin every pooled connection to the orchestrator schema (D5).
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`CREATE SCHEMA IF NOT EXISTS %s`, pgx.Identifier{schema}.Sanitize())); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to create schema %s: %w", schema, err)
	}

	return &Store{pool: pool, schema: schema, dbURL: cfg.DatabaseURL}, nil
}

// Close releases the pool and any held instance lock.
func (s *Store) Close() {
	s.lockMu.Lock()
	if s.lockConn != nil {
		ctx := context.Background()
		_, _ = s.lockConn.Exec(ctx, "SELECT pg_advisory_unlock($1)", s.lockKey())
		_ = s.lockConn.Close(ctx)
		s.lockConn = nil
	}
	s.lockMu.Unlock()
	s.pool.Close()
}

// Schema returns the schema the store operates in.
func (s *Store) Schema() string { return s.schema }

// Migrate applies all embedded migrations not yet recorded in
// schema_migrations. Each migration runs in its own transaction; re-running
// is a no-op, so a second start can never re-apply a migration.
func (s *Store) Migrate(ctx context.Context) error {
	// The runner owns schema_migrations; ensure it exists before reading it.
	if _, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT        PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("failed to ensure schema_migrations: %w", err)
	}

	entries, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("failed to list migrations: %w", err)
	}
	sort.Strings(entries)

	applied := make(map[string]bool)
	rows, err := s.pool.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("failed to read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("failed to scan applied migration: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to iterate applied migrations: %w", err)
	}

	for _, name := range entries {
		version := migrationVersion(name)
		if applied[version] {
			continue
		}
		sql, err := migrationsFS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("failed to read migration %s: %w", name, err)
		}
		if err := s.applyMigration(ctx, version, string(sql)); err != nil {
			return fmt.Errorf("failed to apply migration %s: %w", version, err)
		}
	}
	return nil
}

// applyMigration runs one migration file and records its version atomically.
func (s *Store) applyMigration(ctx context.Context, version, sql string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO schema_migrations (version) VALUES ($1)", version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AppliedMigrations returns recorded migration versions in application order.
// The list itself is part of the deployment contract (AC: 迁移全部应用).
func (s *Store) AppliedMigrations(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("failed to read schema_migrations: %w", err)
	}
	defer rows.Close()

	var versions []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("failed to scan migration version: %w", err)
		}
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

// ListTables returns the table names in the store's schema, sorted.
func (s *Store) ListTables(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = $1 ORDER BY table_name`, s.schema)
	if err != nil {
		return nil, fmt.Errorf("failed to list tables in schema %s: %w", s.schema, err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("failed to scan table name: %w", err)
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

// DropSchema drops the store's schema and everything in it.
// Intended for tests and explicit teardown; never called on live data paths.
func (s *Store) DropSchema(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, fmt.Sprintf(
		`DROP SCHEMA IF EXISTS %s CASCADE`, pgx.Identifier{s.schema}.Sanitize()))
	if err != nil {
		return fmt.Errorf("failed to drop schema %s: %w", s.schema, err)
	}
	return nil
}

// TryAcquireInstanceLock takes the single-instance advisory lock. It fails
// with ErrInstanceLocked when another orchestrator holds it, so a second
// start can neither double-run nor race migrations. The lock is held on a
// dedicated standalone connection for the store's lifetime; closing that
// connection releases it, including after a crash.
func (s *Store) TryAcquireInstanceLock(ctx context.Context) error {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()

	if s.lockConn != nil {
		return nil // already held by this store
	}
	connCfg, err := pgx.ParseConfig(s.dbURL)
	if err != nil {
		return fmt.Errorf("failed to parse database URL: %w", err)
	}
	connCfg.RuntimeParams["search_path"] = s.schema
	conn, err := pgx.ConnectConfig(ctx, connCfg)
	if err != nil {
		return fmt.Errorf("failed to connect for instance lock: %w", err)
	}
	var granted bool
	if err := conn.QueryRow(ctx,
		"SELECT pg_try_advisory_lock($1)", s.lockKey()).Scan(&granted); err != nil {
		_ = conn.Close(ctx)
		return fmt.Errorf("failed to try advisory lock: %w", err)
	}
	if !granted {
		_ = conn.Close(ctx)
		return ErrInstanceLocked
	}
	s.lockConn = conn
	return nil
}

// lockKey derives the advisory-lock key from the schema name, so isolated
// schemas (e.g. tests) never contend for the same lock.
func (s *Store) lockKey() int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("kagent-orchestrator-instance:" + s.schema))
	return int64(h.Sum64())
}

// migrationVersion extracts the version label ("0001_init") from a
// migration file path.
func migrationVersion(path string) string {
	base := path
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			base = path[i+1:]
			break
		}
	}
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '.' {
			return base[:i]
		}
	}
	return base
}

// ContentHash returns the hex SHA256 of data. Used for policy version hashes
// so every decision can reference a stable policy version.
func ContentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
