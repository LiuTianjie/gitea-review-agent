// Package store implements model.Store on top of SQLite (modernc.org/sqlite,
// pure Go, no CGO). It persists the job queue, per-PR session state, findings,
// and console-editable settings.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/turning4th/codex-gitea/internal/model"
)

//go:embed schema.sql
var schemaSQL string

// Store is the SQLite-backed implementation of model.Store.
type Store struct {
	db *sql.DB
}

var _ model.Store = (*Store)(nil)

// Open opens (creating if needed) the SQLite database at dbPath, enables WAL,
// and applies the embedded schema.
func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Serialize access: WAL allows concurrent readers but a single writer;
	// pinning one connection avoids "database is locked" under concurrent workers.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign_keys: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrateSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

// querier abstracts over *sql.DB and *sql.Tx so helpers work in or out of a tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// nowRFC3339 returns the current time as an RFC3339 string (UTC).
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// parseTime parses an RFC3339 timestamp, returning the zero time on failure.
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// nullableTime turns a nullable RFC3339 column into a *time.Time.
func nullableTime(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t := parseTime(ns.String)
	return &t
}

func migrateSchema(db *sql.DB) error {
	for _, col := range []struct {
		table string
		name  string
		def   string
	}{
		{table: "findings", name: "title", def: "TEXT"},
		{table: "findings", name: "body", def: "TEXT"},
	} {
		if err := ensureColumn(db, col.table, col.name, col.def); err != nil {
			return err
		}
	}
	return nil
}

func ensureColumn(db *sql.DB, table, name, def string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid       int
			colName   string
			colType   string
			notNull   int
			defaultV  sql.NullString
			primaryKy int
		)
		if err := rows.Scan(&cid, &colName, &colType, &notNull, &defaultV, &primaryKy); err != nil {
			return fmt.Errorf("scan %s schema: %w", table, err)
		}
		if colName == name {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate %s schema: %w", table, err)
	}
	if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + name + ` ` + def); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, name, err)
	}
	return nil
}

// ensureRepo upserts a repos row by (owner,name) and returns its id.
func ensureRepo(ctx context.Context, q querier, owner, name string) (int64, error) {
	if _, err := q.ExecContext(ctx,
		`INSERT INTO repos(owner,name) VALUES(?,?) ON CONFLICT(owner,name) DO NOTHING`,
		owner, name); err != nil {
		return 0, fmt.Errorf("ensure repo: %w", err)
	}
	var id int64
	if err := q.QueryRowContext(ctx,
		`SELECT id FROM repos WHERE owner=? AND name=?`, owner, name).Scan(&id); err != nil {
		return 0, fmt.Errorf("lookup repo id: %w", err)
	}
	return id, nil
}

// ensurePull upserts repos+pulls rows for pr and returns the pull id.
func ensurePull(ctx context.Context, q querier, pr model.PRRef) (int64, error) {
	repoID, err := ensureRepo(ctx, q, pr.Owner, pr.Repo)
	if err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO pulls(repo_id,number,updated_at) VALUES(?,?,?)
		 ON CONFLICT(repo_id,number) DO NOTHING`,
		repoID, pr.Number, nowRFC3339()); err != nil {
		return 0, fmt.Errorf("ensure pull: %w", err)
	}
	var id int64
	if err := q.QueryRowContext(ctx,
		`SELECT id FROM pulls WHERE repo_id=? AND number=?`, repoID, pr.Number).Scan(&id); err != nil {
		return 0, fmt.Errorf("lookup pull id: %w", err)
	}
	return id, nil
}

// lookupRepoID returns the repo id for owner/name, ok=false if absent.
func lookupRepoID(ctx context.Context, q querier, owner, name string) (int64, bool, error) {
	var id int64
	err := q.QueryRowContext(ctx,
		`SELECT id FROM repos WHERE owner=? AND name=?`, owner, name).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}
