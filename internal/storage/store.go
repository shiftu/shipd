package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
)

const schema = `
CREATE TABLE IF NOT EXISTS apps (
  name        TEXT PRIMARY KEY,
  platform    TEXT NOT NULL,
  created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS releases (
  app_name      TEXT NOT NULL,
  version       TEXT NOT NULL,
  channel       TEXT NOT NULL DEFAULT 'stable',
  platform      TEXT NOT NULL DEFAULT 'generic',
  blob_key      TEXT NOT NULL,
  size          INTEGER NOT NULL,
  sha256        TEXT NOT NULL,
  filename      TEXT NOT NULL,
  notes         TEXT NOT NULL DEFAULT '',
  yanked        INTEGER NOT NULL DEFAULT 0,
  yanked_reason TEXT NOT NULL DEFAULT '',
  bundle_id     TEXT NOT NULL DEFAULT '',
  display_name  TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL,
  PRIMARY KEY(app_name, version, channel),
  FOREIGN KEY(app_name) REFERENCES apps(name)
);

CREATE INDEX IF NOT EXISTS idx_releases_app_created
  ON releases(app_name, created_at DESC);

CREATE TABLE IF NOT EXISTS tokens (
  name         TEXT PRIMARY KEY,
  hash         TEXT NOT NULL UNIQUE,
  scope        TEXT NOT NULL DEFAULT 'rw',
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL DEFAULT 0
);
`

// Store combines metadata (SQLite) and blob storage (filesystem).
type Store struct {
	db      *sql.DB
	blobDir string
}

// Open initializes the store at dataDir, creating subdirectories as needed.
//
//	dataDir/
//	  meta.db        SQLite metadata
//	  blobs/         content-addressed blob files
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "blobs"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir blobs: %w", err)
	}
	dbPath := filepath.Join(dataDir, "meta.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, blobDir: filepath.Join(dataDir, "blobs")}, nil
}

// migrate applies idempotent ALTER TABLEs so a DB created by an older shipd
// version picks up newly-added columns. Each statement may legitimately fail
// with "duplicate column" — that just means the migration already ran.
func migrate(db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE releases ADD COLUMN bundle_id    TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE releases ADD COLUMN display_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE releases ADD COLUMN platform     TEXT NOT NULL DEFAULT 'generic'`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// --- Apps ---

type App struct {
	Name      string `json:"name"`
	Platform  string `json:"platform"`
	CreatedAt int64  `json:"created_at"`
}

func (s *Store) UpsertApp(ctx context.Context, name, platform string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO apps(name, platform, created_at) VALUES(?, ?, ?)
		ON CONFLICT(name) DO NOTHING
	`, name, platform, time.Now().Unix())
	return err
}

func (s *Store) GetApp(ctx context.Context, name string) (*App, error) {
	row := s.db.QueryRowContext(ctx, `SELECT name, platform, created_at FROM apps WHERE name = ?`, name)
	var a App
	if err := row.Scan(&a.Name, &a.Platform, &a.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &a, nil
}

func (s *Store) ListApps(ctx context.Context) ([]App, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, platform, created_at FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.Name, &a.Platform, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- Releases ---

type Release struct {
	AppName      string `json:"app"`
	Version      string `json:"version"`
	Channel      string `json:"channel"`
	Platform     string `json:"platform"`
	BlobKey      string `json:"-"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	Filename     string `json:"filename"`
	Notes        string `json:"notes"`
	Yanked       bool   `json:"yanked"`
	YankedReason string `json:"yanked_reason,omitempty"`
	BundleID     string `json:"bundle_id,omitempty"`
	DisplayName  string `json:"display_name,omitempty"`
	CreatedAt    int64  `json:"created_at"`
}

// PutRelease atomically writes the blob and metadata.
// If a release with (app, version, channel) already exists, returns ErrAlreadyExists.
func (s *Store) PutRelease(ctx context.Context, r Release, body io.Reader) (*Release, error) {
	blobKey, size, sum, err := s.writeBlob(body)
	if err != nil {
		return nil, fmt.Errorf("write blob: %w", err)
	}
	r.BlobKey = blobKey
	r.Size = size
	r.SHA256 = sum
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().Unix()
	}
	if r.Channel == "" {
		r.Channel = "stable"
	}

	if r.Platform == "" {
		r.Platform = "generic"
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO releases(app_name, version, channel, platform, blob_key, size, sha256, filename, notes,
		                    bundle_id, display_name, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, r.AppName, r.Version, r.Channel, r.Platform, r.BlobKey, r.Size, r.SHA256, r.Filename, r.Notes,
		r.BundleID, r.DisplayName, r.CreatedAt)
	if err != nil {
		// best-effort blob cleanup; the blob is content-addressed, so leaving it is also fine
		_ = os.Remove(s.blobPath(blobKey))
		if isUniqueViolation(err) {
			return nil, ErrAlreadyExists
		}
		return nil, err
	}
	return &r, nil
}

func (s *Store) GetRelease(ctx context.Context, app, version, channel string) (*Release, error) {
	if channel == "" {
		channel = "stable"
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT app_name, version, channel, platform, blob_key, size, sha256, filename, notes,
		       yanked, yanked_reason, bundle_id, display_name, created_at
		FROM releases
		WHERE app_name = ? AND version = ? AND channel = ?
	`, app, version, channel)
	return scanRelease(row)
}

// LatestRelease returns the most recently uploaded non-yanked release for the given app/channel.
func (s *Store) LatestRelease(ctx context.Context, app, channel string) (*Release, error) {
	if channel == "" {
		channel = "stable"
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT app_name, version, channel, platform, blob_key, size, sha256, filename, notes,
		       yanked, yanked_reason, bundle_id, display_name, created_at
		FROM releases
		WHERE app_name = ? AND channel = ? AND yanked = 0
		ORDER BY created_at DESC, rowid DESC
		LIMIT 1
	`, app, channel)
	return scanRelease(row)
}

func (s *Store) ListReleases(ctx context.Context, app string) ([]Release, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT app_name, version, channel, platform, blob_key, size, sha256, filename, notes,
		       yanked, yanked_reason, bundle_id, display_name, created_at
		FROM releases
		WHERE app_name = ?
		ORDER BY created_at DESC, rowid DESC
	`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		r, err := scanReleaseRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *Store) YankRelease(ctx context.Context, app, version, channel, reason string) error {
	if channel == "" {
		channel = "stable"
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE releases SET yanked = 1, yanked_reason = ?
		WHERE app_name = ? AND version = ? AND channel = ?
	`, reason, app, version, channel)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// OpenBlob returns a reader for the blob backing this release.
func (s *Store) OpenBlob(r *Release) (io.ReadCloser, error) {
	return os.Open(s.blobPath(r.BlobKey))
}

func (s *Store) blobPath(key string) string {
	if len(key) < 2 {
		return filepath.Join(s.blobDir, key)
	}
	return filepath.Join(s.blobDir, key[:2], key[2:])
}

// writeBlob streams body to a temp file, computes sha256, then renames into a
// content-addressed path. Returns the blob key (sha256 hex), size, and digest.
func (s *Store) writeBlob(body io.Reader) (string, int64, string, error) {
	tmp, err := os.CreateTemp(s.blobDir, "upload-*")
	if err != nil {
		return "", 0, "", err
	}
	tmpName := tmp.Name()
	defer func() {
		// remove tmp if it still exists (i.e. rename did not happen)
		_ = os.Remove(tmpName)
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), body)
	if err != nil {
		_ = tmp.Close()
		return "", 0, "", err
	}
	if err := tmp.Close(); err != nil {
		return "", 0, "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	dst := s.blobPath(sum)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, "", err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", 0, "", err
	}
	return sum, n, sum, nil
}

// --- Tokens ---

type Token struct {
	Name       string `json:"name"`
	Scope      string `json:"scope"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at"`
}

// CreateToken stores a hashed token. The plaintext value is generated by the
// caller and shown to the user once.
func (s *Store) CreateToken(ctx context.Context, name, plaintext, scope string) error {
	if scope == "" {
		scope = "rw"
	}
	hash := hashToken(plaintext)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tokens(name, hash, scope, created_at) VALUES(?, ?, ?, ?)
	`, name, hash, scope, time.Now().Unix())
	if isUniqueViolation(err) {
		return ErrAlreadyExists
	}
	return err
}

func (s *Store) ListTokens(ctx context.Context) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, scope, created_at, last_used_at FROM tokens ORDER BY created_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.Name, &t.Scope, &t.CreatedAt, &t.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) RevokeToken(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// LookupToken returns the token row matching plaintext, or ErrNotFound.
func (s *Store) LookupToken(ctx context.Context, plaintext string) (*Token, error) {
	hash := hashToken(plaintext)
	row := s.db.QueryRowContext(ctx, `
		SELECT name, scope, created_at, last_used_at FROM tokens WHERE hash = ?
	`, hash)
	var t Token
	if err := row.Scan(&t.Name, &t.Scope, &t.CreatedAt, &t.LastUsedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE hash = ?`, time.Now().Unix(), hash)
	return &t, nil
}

func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// --- helpers ---

type scanner interface {
	Scan(dest ...any) error
}

func scanRelease(s scanner) (*Release, error) {
	var r Release
	var yanked int
	if err := s.Scan(&r.AppName, &r.Version, &r.Channel, &r.Platform, &r.BlobKey, &r.Size, &r.SHA256,
		&r.Filename, &r.Notes, &yanked, &r.YankedReason,
		&r.BundleID, &r.DisplayName, &r.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	r.Yanked = yanked == 1
	return &r, nil
}

func scanReleaseRow(rows *sql.Rows) (*Release, error) { return scanRelease(rows) }

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
