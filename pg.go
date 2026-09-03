package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgConfig holds the Postgres connection parameters for the repo-extension's
// private database (never the agent's database — the agent migrates its own
// schema and would drop foreign tables).
type PgConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	DB       string
}

func (c PgConfig) dsn(db string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s", c.User, c.Password, c.Host, c.Port, db)
}

// MapRow is one row of the bookmark<->session mapping table.
type MapRow struct {
	Org         string
	Repo        string
	Bookmark    string
	SessionName string
}

// Store is the repo-extension persistence layer over pgx.
type Store struct {
	pool *pgxpool.Pool
}

// OpenStore ensures the database exists (best-effort; operators may
// pre-provision it), then opens a pool and applies the schema DDL.
func OpenStore(ctx context.Context, cfg PgConfig) (*Store, error) {
	if err := ensureDatabase(ctx, cfg); err != nil {
		log.Warn("ensure database failed (continuing; it may be pre-provisioned)", "db", cfg.DB, "err", err)
	}
	pool, err := pgxpool.New(ctx, cfg.dsn(cfg.DB))
	if err != nil {
		return nil, fmt.Errorf("pg connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pg ping: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() { s.pool.Close() }

// ensureDatabase creates cfg.DB when missing. Requires CREATE DATABASE
// privileges on the maintenance DB; failures are non-fatal.
func ensureDatabase(ctx context.Context, cfg PgConfig) error {
	pool, err := pgxpool.New(ctx, cfg.dsn("postgres"))
	if err != nil {
		return err
	}
	defer pool.Close()
	var one int
	scanErr := pool.QueryRow(ctx, `SELECT 1 FROM pg_database WHERE datname = $1`, cfg.DB).Scan(&one)
	switch {
	case errors.Is(scanErr, pgx.ErrNoRows): // missing → create below
	case scanErr != nil:
		return scanErr
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, cfg.DB)); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "42P04" { // duplicate_database
			return nil
		}
		return err
	}
	log.Info("created database", "db", cfg.DB)
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS managed_repos (
  org        TEXT NOT NULL,
  repo       TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (org, repo)
);
CREATE TABLE IF NOT EXISTS session_repos (
  org          TEXT NOT NULL,
  repo         TEXT NOT NULL,
  bookmark     TEXT NOT NULL,
  session_name TEXT NOT NULL UNIQUE,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (org, repo, bookmark)
);
CREATE TABLE IF NOT EXISTS executed_worksheets (
  worksheet_id TEXT PRIMARY KEY,
  action       TEXT NOT NULL DEFAULT '',
  session_name TEXT NOT NULL DEFAULT '',
  executed_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);`
	_, err := s.pool.Exec(ctx, ddl)
	if err != nil {
		return fmt.Errorf("ddl: %w", err)
	}
	return nil
}

// ---- managed repos ----

func (s *Store) IsManaged(ctx context.Context, org, repo string) (bool, error) {
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM managed_repos WHERE org=$1 AND repo=$2`, org, repo).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) InsertManaged(ctx context.Context, org, repo string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO managed_repos (org, repo) VALUES ($1, $2) ON CONFLICT DO NOTHING`, org, repo)
	return err
}

func (s *Store) DeleteManaged(ctx context.Context, org, repo string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM managed_repos WHERE org=$1 AND repo=$2`, org, repo)
	return err
}

func (s *Store) ListManaged(ctx context.Context) ([]MapRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT org, repo FROM managed_repos ORDER BY org, repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MapRow
	for rows.Next() {
		var r MapRow
		if err := rows.Scan(&r.Org, &r.Repo); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- mapping rows ----

const mapCols = `org, repo, bookmark, session_name`

func scanMapRow(row pgx.Row) (*MapRow, error) {
	var r MapRow
	err := row.Scan(&r.Org, &r.Repo, &r.Bookmark, &r.SessionName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) GetRow(ctx context.Context, org, repo, bookmark string) (*MapRow, error) {
	return scanMapRow(s.pool.QueryRow(ctx,
		`SELECT `+mapCols+` FROM session_repos WHERE org=$1 AND repo=$2 AND bookmark=$3`,
		org, repo, bookmark))
}

func (s *Store) GetRowBySession(ctx context.Context, sessionName string) (*MapRow, error) {
	return scanMapRow(s.pool.QueryRow(ctx,
		`SELECT `+mapCols+` FROM session_repos WHERE session_name=$1`, sessionName))
}

// InsertRow records a mapping. A unique violation is returned as
// errConflict so callers can surface 409.
func (s *Store) InsertRow(ctx context.Context, org, repo, bookmark, sessionName string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO session_repos (org, repo, bookmark, session_name) VALUES ($1, $2, $3, $4)`,
		org, repo, bookmark, sessionName)
	if isUniqueViolation(err) {
		return errConflict("bookmark or session already bound")
	}
	return err
}

// RenameRow moves a mapping row to a new bookmark + session name.
func (s *Store) RenameRow(ctx context.Context, org, repo, fromBM, toBM, toSession string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE session_repos SET bookmark=$4, session_name=$5, updated_at=now()
		 WHERE org=$1 AND repo=$2 AND bookmark=$3`,
		org, repo, fromBM, toBM, toSession)
	if err != nil {
		if isUniqueViolation(err) {
			return errConflict("target bookmark or session already bound")
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return errNotFound("mapping row not found")
	}
	return nil
}

func (s *Store) DeleteRow(ctx context.Context, org, repo, bookmark string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM session_repos WHERE org=$1 AND repo=$2 AND bookmark=$3`, org, repo, bookmark)
	return err
}

// DeleteRowsForRepo removes every mapping row of one repo (delete-repo path).
func (s *Store) DeleteRowsForRepo(ctx context.Context, org, repo string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM session_repos WHERE org=$1 AND repo=$2`, org, repo)
	return err
}

func (s *Store) ListRowsForRepo(ctx context.Context, org, repo string) ([]MapRow, error) {
	return s.listRows(ctx, `SELECT `+mapCols+` FROM session_repos WHERE org=$1 AND repo=$2 ORDER BY bookmark`, org, repo)
}

func (s *Store) ListRows(ctx context.Context) ([]MapRow, error) {
	return s.listRows(ctx, `SELECT `+mapCols+` FROM session_repos ORDER BY org, repo, bookmark`)
}

func (s *Store) listRows(ctx context.Context, q string, args ...interface{}) ([]MapRow, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MapRow
	for rows.Next() {
		var r MapRow
		if err := rows.Scan(&r.Org, &r.Repo, &r.Bookmark, &r.SessionName); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- error helpers ----

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// ---- worksheet dedup ----

// ExecutedWorksheet records a worksheet as executed (at-least-once dispatch
// guard). Returns true when this call was the first to claim it; false means
// a prior execution already ran and side effects must be skipped.
func (s *Store) ExecutedWorksheet(ctx context.Context, worksheetID, action, sessionName string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
INSERT INTO executed_worksheets (worksheet_id, action, session_name)
VALUES ($1, $2, $3)
ON CONFLICT (worksheet_id) DO NOTHING`, worksheetID, action, sessionName)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
