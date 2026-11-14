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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrInstanceLocked is returned when another orchestrator instance already
// holds the instance lock (docs/designs/0001 §9: P0 is single-replica).
var ErrInstanceLocked = errors.New("instance lock held by another orchestrator")

// ErrPolicyNotFound is returned by LatestPolicyVersion when no policy
// version has been recorded yet.
var ErrPolicyNotFound = errors.New("no policy version recorded")

// ErrInboxLocked is returned by IngestEvents when another worker already
// holds the source's inbox claim. An inbox has a single active worker at a
// time (docs/designs/0004 互证表: 单 worker); a refused claimant must skip the
// cycle and retry later — overlapping polls are safe because triggers dedup.
var ErrInboxLocked = errors.New("inbox already claimed by another worker")

// ErrInvalidEvent rejects events that would create unrepresentable state.
var ErrInvalidEvent = errors.New("invalid event")

// SourceRef identifies a Source instance (docs/designs/0001 §3: 读位置、bot
// 身份的拥有者). Name is globally unique, e.g. "github:owner/repo".
type SourceRef struct {
	Name string
	Kind string
}

// Event is one external event to ingest, transport-agnostic. Intake builds
// Events from transport payloads; the store persists them opaquely.
type Event struct {
	// ThreadKey is the external conversation key within the source
	// (e.g. "issue:42").
	ThreadKey string
	// DeterministicID is the immutable event identity within the source.
	// (source, deterministic_id) deduplicates trigger rows.
	DeterministicID string
	// Payload is the verbatim audit payload (JSON).
	Payload []byte
	// OccurredAt is the external event time. It drives the thread sightline
	// and the per-source cursor high-water mark.
	OccurredAt time.Time
}

func (e Event) validate() error {
	switch {
	case e.DeterministicID == "":
		return fmt.Errorf("%w: deterministic id is empty", ErrInvalidEvent)
	case e.ThreadKey == "":
		return fmt.Errorf("%w: thread key is empty", ErrInvalidEvent)
	case len(e.Payload) == 0:
		return fmt.Errorf("%w: payload is empty", ErrInvalidEvent)
	case e.OccurredAt.IsZero():
		return fmt.Errorf("%w: occurred time is zero", ErrInvalidEvent)
	}
	return nil
}

// IngestStats reports what one IngestEvents batch did.
type IngestStats struct {
	// Ingested counts triggers actually inserted.
	Ingested int
	// Duplicates counts events whose (source, deterministic_id) row already
	// existed — the overlap window absorbed by dedup.
	Duplicates int
	// Dropped counts events ignored because their thread is beyond the
	// sightline (no live ticket and no ingested activity within the window).
	Dropped int
	// Cursor is the persisted cursor after the batch: the occurrence
	// high-water mark, never regressing across batches.
	Cursor time.Time
}

// Trigger is one immutable ingested event row (audit and idempotency unit).
type Trigger struct {
	Source          string
	DeterministicID string
	ThreadKey       string
	Payload         []byte
	CreatedAt       time.Time
}

// PolicySnapshot is the latest recorded policy version.
type PolicySnapshot struct {
	Version   string
	Content   []byte
	AppliedAt time.Time
}

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
	// PostgreSQL identifiers truncate at 63 bytes, but search_path elements
	// longer than that silently fail to resolve ("no schema has been
	// selected to create in"). Refuse such names instead of mis-resolving.
	if len(schema) > 63 {
		return nil, fmt.Errorf("schema name %q is %d bytes; PostgreSQL resolves at most 63", schema, len(schema))
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

// RecordPolicyVersion persists a policy version hash (idempotent on the
// version). Every later Gate decision and Mandate references this hash
// (docs/designs/0001 §5: 策略分发).
func (s *Store) RecordPolicyVersion(ctx context.Context, version string, content []byte) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO policy (version, content)
		VALUES ($1, $2)
		ON CONFLICT (version) DO NOTHING`,
		version, content); err != nil {
		return fmt.Errorf("failed to record policy version %s: %w", version, err)
	}
	return nil
}

// LatestPolicyVersion returns the most recently recorded policy version.
// "Most recent" is decided by insertion order, not wall-clock timestamps,
// because two versions can be applied within the same microsecond.
func (s *Store) LatestPolicyVersion(ctx context.Context) (PolicySnapshot, error) {
	var snap PolicySnapshot
	err := s.pool.QueryRow(ctx, `
		SELECT version, content, applied_at
		FROM policy
		ORDER BY id DESC
		LIMIT 1`).Scan(&snap.Version, &snap.Content, &snap.AppliedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicySnapshot{}, ErrPolicyNotFound
	}
	if err != nil {
		return PolicySnapshot{}, fmt.Errorf("failed to read latest policy version: %w", err)
	}
	return snap, nil
}

// IngestEvents ingests one batch of external events atomically (docs/designs
// 0001 §6): it claims the source's inbox, applies the thread sightline,
// inserts triggers with (source, deterministic_id) dedup, and advances the
// per-source cursor — all in a single transaction, so a crash mid-batch
// persists neither triggers nor cursor and the next poll re-reads the window.
//
// The cursor high-water mark covers every event in the batch (ingested,
// duplicate, or dropped): they were all read, so the next poll must not
// rewind past them. Duplicate and dropped events never refresh the thread's
// activity, so a sightline-expired thread cannot resurrect itself.
func (s *Store) IngestEvents(ctx context.Context, src SourceRef, events []Event, sightline time.Duration) (IngestStats, error) {
	if len(events) == 0 {
		return IngestStats{}, nil
	}
	for i, e := range events {
		if err := e.validate(); err != nil {
			return IngestStats{}, fmt.Errorf("event %d: %w", i, err)
		}
	}

	// The sightline cutoff comes from wall-clock time, not the events: a
	// thread is visible when its last ingested activity is newer than the
	// cutoff or it holds a live ticket.
	cutoff := time.Now().Add(-sightline)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return IngestStats{}, fmt.Errorf("failed to begin ingest transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Single active worker per inbox: a transaction-scoped advisory claim.
	// It auto-releases at commit/rollback, including after a crash.
	var claimed bool
	if err := tx.QueryRow(ctx,
		"SELECT pg_try_advisory_xact_lock($1)", s.inboxKey(src.Name)).Scan(&claimed); err != nil {
		return IngestStats{}, fmt.Errorf("failed to claim inbox %s: %w", src.Name, err)
	}
	if !claimed {
		return IngestStats{}, ErrInboxLocked
	}

	stats := IngestStats{}
	for _, e := range events {
		// Thread attribution: create the thread at first sight with the
		// event's occurrence time; an existing thread keeps its history.
		if _, err := tx.Exec(ctx, `
			INSERT INTO threads (source, thread_key, last_activity_at)
			VALUES ($1, $2, $3)
			ON CONFLICT (source, thread_key) DO NOTHING`,
			src.Name, e.ThreadKey, e.OccurredAt); err != nil {
			return IngestStats{}, fmt.Errorf("failed to ensure thread %s/%s: %w", src.Name, e.ThreadKey, err)
		}

		// Sightline: ingest only threads that hold a live ticket or were
		// ingested within the sightline window.
		var visible bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM threads th
				WHERE th.source = $1 AND th.thread_key = $2
				  AND (th.last_activity_at > $3
				       OR EXISTS (SELECT 1 FROM tickets tk
				                  WHERE tk.thread_source = th.source
				                    AND tk.thread_key = th.thread_key
				                    AND tk.status <> $4))
			)`, src.Name, e.ThreadKey, cutoff, TicketStatusEnded).Scan(&visible); err != nil {
			return IngestStats{}, fmt.Errorf("failed to check sightline for thread %s/%s: %w", src.Name, e.ThreadKey, err)
		}
		if !visible {
			stats.Dropped++
			continue
		}

		// Trigger dedup: the primary key (source, deterministic_id) absorbs
		// the poll overlap window (docs/designs/0001 §6).
		ct, err := tx.Exec(ctx, `
			INSERT INTO triggers (source, deterministic_id, thread_key, payload)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (source, deterministic_id) DO NOTHING`,
			src.Name, e.DeterministicID, e.ThreadKey, e.Payload)
		if err != nil {
			return IngestStats{}, fmt.Errorf("failed to insert trigger %s: %w", e.DeterministicID, err)
		}
		if ct.RowsAffected() == 1 {
			stats.Ingested++
			// Only a real ingestion refreshes the sightline, and only forward.
			if _, err := tx.Exec(ctx, `
				UPDATE threads SET last_activity_at = GREATEST(last_activity_at, $3)
				WHERE source = $1 AND thread_key = $2`,
				src.Name, e.ThreadKey, e.OccurredAt); err != nil {
				return IngestStats{}, fmt.Errorf("failed to refresh thread activity %s/%s: %w", src.Name, e.ThreadKey, err)
			}
		} else {
			stats.Duplicates++
		}
	}

	// Cursor advance: monotonic per source, persisted in the same transaction
	// as the triggers (restart resumes exactly from the committed position).
	if _, err := tx.Exec(ctx, `
		INSERT INTO sources (name, kind, cursor, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (name) DO UPDATE
		SET cursor = GREATEST(COALESCE(sources.cursor, EXCLUDED.cursor), EXCLUDED.cursor),
		    updated_at = now()`,
		src.Name, src.Kind, batchMax(events)); err != nil {
		return IngestStats{}, fmt.Errorf("failed to persist cursor for %s: %w", src.Name, err)
	}
	var cursor time.Time
	if err := tx.QueryRow(ctx,
		"SELECT cursor FROM sources WHERE name = $1", src.Name).Scan(&cursor); err != nil {
		return IngestStats{}, fmt.Errorf("failed to read back cursor for %s: %w", src.Name, err)
	}
	stats.Cursor = cursor

	if err := tx.Commit(ctx); err != nil {
		return IngestStats{}, fmt.Errorf("failed to commit ingest: %w", err)
	}
	return stats, nil
}

// batchMax returns the newest occurrence time in a batch.
func batchMax(events []Event) time.Time {
	max := events[0].OccurredAt
	for _, e := range events[1:] {
		if e.OccurredAt.After(max) {
			max = e.OccurredAt
		}
	}
	return max
}

// SourceCursor returns the persisted read position of a source. ok is false
// when the source never ingested a batch or never advanced its cursor.
func (s *Store) SourceCursor(ctx context.Context, source string) (time.Time, bool, error) {
	var cursor *time.Time
	err := s.pool.QueryRow(ctx,
		"SELECT cursor FROM sources WHERE name = $1", source).Scan(&cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("failed to read cursor for %s: %w", source, err)
	}
	if cursor == nil {
		return time.Time{}, false, nil
	}
	return *cursor, true, nil
}

// Triggers returns the audit rows ingested for a source, oldest first.
// The trigger table is the audit and idempotency unit (docs/designs/0001 §3),
// so replaying "who triggered what" starts here.
func (s *Store) Triggers(ctx context.Context, source string) ([]Trigger, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT source, deterministic_id, thread_key, payload, created_at
		FROM triggers
		WHERE source = $1
		ORDER BY created_at, deterministic_id`, source)
	if err != nil {
		return nil, fmt.Errorf("failed to list triggers for %s: %w", source, err)
	}
	defer rows.Close()

	var out []Trigger
	for rows.Next() {
		var tr Trigger
		if err := rows.Scan(&tr.Source, &tr.DeterministicID, &tr.ThreadKey, &tr.Payload, &tr.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan trigger: %w", err)
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// inboxKey derives the per-inbox advisory-lock key from the schema and source
// name, so isolated schemas (e.g. tests) never contend for the same lock.
func (s *Store) inboxKey(source string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("kagent-orchestrator-inbox:" + s.schema + ":" + source))
	return int64(h.Sum64())
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
