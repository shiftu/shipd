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
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrExpired       = errors.New("expired")
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
  last_used_at INTEGER NOT NULL DEFAULT 0,
  expires_at   INTEGER NOT NULL DEFAULT 0
);
`

// Store combines metadata (SQLite) and blob storage (an arbitrary BlobStore).
type Store struct {
	db    *sql.DB
	blobs BlobStore
}

// Open initializes the store at dataDir, creating subdirectories as needed.
// If blobs is nil, the default filesystem backend at <dataDir>/blobs is used —
// preserving the zero-config behavior that's been there since v0.1.
//
//	dataDir/
//	  meta.db        SQLite metadata
//	  blobs/         content-addressed blobs (FS backend default)
func Open(dataDir string, blobs BlobStore) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir data: %w", err)
	}
	if blobs == nil {
		fs, err := NewFSBlobStore(filepath.Join(dataDir, "blobs"))
		if err != nil {
			return nil, err
		}
		blobs = fs
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
	return &Store{db: db, blobs: blobs}, nil
}

// migrate applies idempotent ALTER TABLEs so a DB created by an older shipd
// version picks up newly-added columns. Each statement may legitimately fail
// with "duplicate column" — that just means the migration already ran.
func migrate(db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE releases ADD COLUMN bundle_id    TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE releases ADD COLUMN display_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE releases ADD COLUMN platform     TEXT NOT NULL DEFAULT 'generic'`,
		`ALTER TABLE tokens   ADD COLUMN expires_at   INTEGER NOT NULL DEFAULT 0`,
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
	if r.Channel == "" {
		r.Channel = "stable"
	}
	if r.Platform == "" {
		r.Platform = "generic"
	}

	// Cheap pre-check before reading the body: if (app, version, channel) is
	// already taken, refuse without uploading. A multi-GB blob upload — and on
	// S3, a real PutObject bill — for a CI job that retries the same publish
	// is otherwise wasted. The UNIQUE constraint on the INSERT below is still
	// the source of truth for the race where two concurrent publishes both
	// pass this check; this just covers the common case.
	var exists int
	switch err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM releases WHERE app_name = ? AND version = ? AND channel = ?
	`, r.AppName, r.Version, r.Channel).Scan(&exists); {
	case err == nil:
		return nil, ErrAlreadyExists
	case errors.Is(err, sql.ErrNoRows):
		// fall through
	default:
		return nil, fmt.Errorf("check existing release: %w", err)
	}

	blobKey, size, sum, err := s.blobs.Put(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("write blob: %w", err)
	}
	r.BlobKey = blobKey
	r.Size = size
	r.SHA256 = sum
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().Unix()
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO releases(app_name, version, channel, platform, blob_key, size, sha256, filename, notes,
		                    bundle_id, display_name, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, r.AppName, r.Version, r.Channel, r.Platform, r.BlobKey, r.Size, r.SHA256, r.Filename, r.Notes,
		r.BundleID, r.DisplayName, r.CreatedAt)
	if err != nil {
		// Content-addressed blobs are safe to leave behind on a metadata
		// failure: a future PutRelease with the same content will collapse
		// onto the same key. Skip explicit blob cleanup — the cost across
		// backends (especially S3) outweighs the benefit.
		_ = blobKey
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

// LatestReleases returns the latest non-yanked release per distinct platform
// on the given channel, sorted alphabetically by platform.
//
// This is the multi-platform-friendly counterpart to LatestRelease. When an
// app has builds for both iOS and Android (different versions on the same
// channel), LatestRelease returns whichever was uploaded most recently —
// which surprises operators who expect "/install/{app}" to surface every
// platform. LatestReleases lets the install page render one button per
// available platform.
//
// Implementation: walk ListReleases (already sorted by created_at DESC) and
// keep the first hit per platform. The N here is one app's release history,
// not millions of rows; sorting in Go is comfortable.
func (s *Store) LatestReleases(ctx context.Context, app, channel string) ([]Release, error) {
	if channel == "" {
		channel = "stable"
	}
	all, err := s.ListReleases(ctx, app)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Release
	for _, r := range all {
		if r.Channel != channel || r.Yanked {
			continue
		}
		if seen[r.Platform] {
			continue
		}
		seen[r.Platform] = true
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Platform < out[j].Platform })
	return out, nil
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

// PromoteRelease creates a new release row on dstChannel that points at the
// same blob as the existing source release. No bytes are copied — the
// content-addressed blob is shared. Notes, bundle_id, display_name, platform,
// filename, size, and sha256 carry over from the source. The new row starts
// non-yanked even if the source was yanked-on-its-channel — but a yanked
// source is refused outright since promoting a known-bad build is almost
// always a mistake.
//
// If srcChannel is empty, the source must exist on exactly one channel; if
// the version exists on multiple channels, the call returns an error and
// asks the caller to pass an explicit srcChannel.
//
// Errors:
//   - ErrNotFound      — no row for (app, version) (or (app, version, srcChannel) when given)
//   - ErrAlreadyExists — (app, version, dstChannel) is already taken
//   - other            — yanked source, same-channel promotion, ambiguous source
func (s *Store) PromoteRelease(ctx context.Context, app, version, srcChannel, dstChannel string) (*Release, error) {
	if dstChannel == "" {
		return nil, errors.New("destination channel is required")
	}

	var src *Release
	if srcChannel == "" {
		rels, err := s.releasesForVersion(ctx, app, version)
		if err != nil {
			return nil, err
		}
		if len(rels) == 0 {
			return nil, ErrNotFound
		}
		if len(rels) > 1 {
			return nil, fmt.Errorf("version %s exists on multiple channels (%s); pass an explicit source channel",
				version, channelList(rels))
		}
		src = &rels[0]
	} else {
		var err error
		src, err = s.GetRelease(ctx, app, version, srcChannel)
		if err != nil {
			return nil, err
		}
	}

	if src.Channel == dstChannel {
		return nil, fmt.Errorf("source and destination channel are the same (%s)", dstChannel)
	}
	if src.Yanked {
		return nil, fmt.Errorf("source release %s@%s [%s] is yanked", app, version, src.Channel)
	}

	dst := *src
	dst.Channel = dstChannel
	dst.Yanked = false
	dst.YankedReason = ""
	dst.CreatedAt = time.Now().Unix()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO releases(app_name, version, channel, platform, blob_key, size, sha256,
		                    filename, notes, bundle_id, display_name, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, dst.AppName, dst.Version, dst.Channel, dst.Platform, dst.BlobKey, dst.Size, dst.SHA256,
		dst.Filename, dst.Notes, dst.BundleID, dst.DisplayName, dst.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrAlreadyExists
		}
		return nil, err
	}
	return &dst, nil
}

// releasesForVersion returns every channel-row for (app, version), used by
// PromoteRelease's auto-detect path.
func (s *Store) releasesForVersion(ctx context.Context, app, version string) ([]Release, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT app_name, version, channel, platform, blob_key, size, sha256, filename, notes,
		       yanked, yanked_reason, bundle_id, display_name, created_at
		FROM releases
		WHERE app_name = ? AND version = ?
		ORDER BY channel
	`, app, version)
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

func channelList(rels []Release) string {
	names := make([]string, 0, len(rels))
	for _, r := range rels {
		names = append(names, r.Channel)
	}
	return strings.Join(names, ", ")
}

// OpenBlob returns a reader for the blob backing this release.
func (s *Store) OpenBlob(r *Release) (io.ReadCloser, error) {
	return s.blobs.Get(context.Background(), r.BlobKey)
}

// --- Tokens ---

type Token struct {
	Name       string `json:"name"`
	Scope      string `json:"scope"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at,omitempty"` // 0 = never expires
	LastUsedAt int64  `json:"last_used_at"`
}

// CreateToken stores a hashed token. The plaintext value is generated by the
// caller and shown to the user once. expiresAt is the unix-second deadline
// after which LookupToken rejects the token; pass 0 for "never expires".
func (s *Store) CreateToken(ctx context.Context, name, plaintext, scope string, expiresAt int64) error {
	if scope == "" {
		scope = "rw"
	}
	hash := hashToken(plaintext)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tokens(name, hash, scope, created_at, expires_at) VALUES(?, ?, ?, ?, ?)
	`, name, hash, scope, time.Now().Unix(), expiresAt)
	if isUniqueViolation(err) {
		return ErrAlreadyExists
	}
	return err
}

func (s *Store) ListTokens(ctx context.Context) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, scope, created_at, expires_at, last_used_at FROM tokens ORDER BY created_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.Name, &t.Scope, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt); err != nil {
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

// LookupToken returns the token row matching plaintext.
// Returns ErrNotFound if no token matches, ErrExpired if the matching token
// has a non-zero expires_at in the past. last_used_at is updated only on a
// successful lookup so expired-token probes don't refresh the timestamp.
func (s *Store) LookupToken(ctx context.Context, plaintext string) (*Token, error) {
	hash := hashToken(plaintext)
	row := s.db.QueryRowContext(ctx, `
		SELECT name, scope, created_at, expires_at, last_used_at FROM tokens WHERE hash = ?
	`, hash)
	var t Token
	if err := row.Scan(&t.Name, &t.Scope, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if t.ExpiresAt != 0 && time.Now().Unix() > t.ExpiresAt {
		return nil, ErrExpired
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
