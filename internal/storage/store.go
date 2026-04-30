package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
  yanked_at     INTEGER NOT NULL DEFAULT 0,
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
		`ALTER TABLE releases ADD COLUMN yanked_at    INTEGER NOT NULL DEFAULT 0`,
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
	YankedAt     int64  `json:"yanked_at,omitempty"`
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
		       yanked, yanked_reason, yanked_at, bundle_id, display_name, created_at
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
		       yanked, yanked_reason, yanked_at, bundle_id, display_name, created_at
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
		       yanked, yanked_reason, yanked_at, bundle_id, display_name, created_at
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
		UPDATE releases SET yanked = 1, yanked_reason = ?, yanked_at = ?
		WHERE app_name = ? AND version = ? AND channel = ?
	`, reason, time.Now().Unix(), app, version, channel)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GCCandidates returns yanked releases whose yank happened more than
// olderThan ago AND that aren't among the keepLast most-recently-published
// rows in their (app, channel, platform) group — i.e., the rows whose blobs
// `shipd gc --delete` would reclaim.
//
//   - olderThan == 0: any age (even just-yanked) is eligible.
//   - keepLast == 0: no per-group safety net; full cleanup.
//   - keepLast > 0:  the N most recent rows per (app, channel, platform),
//     regardless of yank state, are protected. This prevents a sequence of
//     yanks from eventually emptying out an app's storage entirely — even
//     for low-traffic apps where every release eventually accumulates a
//     yank, the most-recent artifact bytes survive so an operator who
//     accidentally over-yanked can recover by un-yanking that row.
//
// Releases yanked before the yanked_at column existed have yanked_at = 0,
// which sorts as "the dawn of time" and is therefore always old-enough —
// pre-migration yanks become eligible automatically once they fall out of
// the keepLast window.
//
// Returned candidates are sorted by (yanked_at, app, version) for stable
// dry-run output. Per-group ranking happens in Go because SQLite's window
// functions are usable but make the query harder to read for the
// release-history sizes shipd actually deals with (dozens, not millions).
func (s *Store) GCCandidates(ctx context.Context, olderThan time.Duration, keepLast int) ([]Release, error) {
	cutoff := time.Now().Add(-olderThan).Unix()

	// One pass over all rows, ordered so each (app, channel, platform) group
	// arrives newest-first. Walk the group, count rank, and only flag rows
	// past keepLast that are also yanked-and-old.
	rows, err := s.db.QueryContext(ctx, `
		SELECT app_name, version, channel, platform, blob_key, size, sha256, filename, notes,
		       yanked, yanked_reason, yanked_at, bundle_id, display_name, created_at
		FROM releases
		ORDER BY app_name, channel, platform, created_at DESC, rowid DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		out       []Release
		lastGroup string
		rank      int
	)
	for rows.Next() {
		r, err := scanReleaseRow(rows)
		if err != nil {
			return nil, err
		}
		group := r.AppName + "\x00" + r.Channel + "\x00" + r.Platform
		if group != lastGroup {
			lastGroup = group
			rank = 0
		}
		rank++
		if rank <= keepLast {
			continue
		}
		if !r.Yanked || r.YankedAt > cutoff {
			continue
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].YankedAt != out[j].YankedAt {
			return out[i].YankedAt < out[j].YankedAt
		}
		if out[i].AppName != out[j].AppName {
			return out[i].AppName < out[j].AppName
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}

// DeleteReleaseAndBlob removes a release row and, if no other release rows
// reference the same content-addressed blob, deletes the blob too. Returns
// whether the blob was actually deleted (false means it was kept because
// another release shares it via dedup).
//
// Order of operations matters: the metadata row is deleted first, then
// remaining references are counted. If a blob delete fails (transient S3
// or FS error), the metadata is already gone and the orphaned bytes will
// linger — accepted as a small cost since orphan blobs are storage-bill
// noise but a row pointing at missing bytes would be a download error
// surfaced to users. Operators can re-run gc later; the row is gone so a
// second pass won't re-process this release.
//
// This is destructive: any pinned download URL bound to (app, version,
// channel) will return 404 after this call. The CLI gates it behind an
// explicit --delete flag.
func (s *Store) DeleteReleaseAndBlob(ctx context.Context, app, version, channel string) (blobDeleted bool, err error) {
	if channel == "" {
		channel = "stable"
	}

	var blobKey string
	if err := s.db.QueryRowContext(ctx, `
		SELECT blob_key FROM releases WHERE app_name = ? AND version = ? AND channel = ?
	`, app, version, channel).Scan(&blobKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}

	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM releases WHERE app_name = ? AND version = ? AND channel = ?
	`, app, version, channel); err != nil {
		return false, err
	}

	var refs int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM releases WHERE blob_key = ?`, blobKey).Scan(&refs); err != nil {
		return false, fmt.Errorf("count remaining refs: %w", err)
	}
	if refs > 0 {
		// Dedup'd: another release still backs onto this blob. Keep the bytes.
		return false, nil
	}

	if err := s.blobs.Delete(ctx, blobKey); err != nil {
		return false, fmt.Errorf("delete blob %s: %w", blobKey, err)
	}
	return true, nil
}

// UnyankRelease reverses YankRelease: clears yanked, yanked_reason, and
// yanked_at on (app, version, channel). Idempotent — calling on a release
// that isn't yanked is not an error. Returns ErrNotFound only when no row
// matches.
//
// This is the recovery path for keep-last in gc: a yanked-but-bytes-kept
// row can be brought back to live status without a re-publish.
func (s *Store) UnyankRelease(ctx context.Context, app, version, channel string) error {
	if channel == "" {
		channel = "stable"
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE releases SET yanked = 0, yanked_reason = '', yanked_at = 0
		WHERE app_name = ? AND version = ? AND channel = ?
	`, app, version, channel)
	if err != nil {
		return err
	}
	// 0 rows affected can mean "not found" OR "already not yanked". Probe
	// existence to disambiguate so the operator gets a useful error.
	if n, _ := res.RowsAffected(); n == 0 {
		var dummy int
		row := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM releases WHERE app_name = ? AND version = ? AND channel = ?`,
			app, version, channel)
		if err := row.Scan(&dummy); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		// Row exists, just wasn't yanked — idempotent success.
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
		       yanked, yanked_reason, yanked_at, bundle_id, display_name, created_at
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

// StorageStats is the snapshot of catalog-wide counts and bytes that the
// /metrics endpoint exposes as gauges. BlobBytesUnique counts each
// content-addressed blob once even when multiple release rows reference it,
// so it tracks actual disk usage rather than referenced bytes.
type StorageStats struct {
	Apps            int64
	ReleasesLive    int64
	ReleasesYanked  int64
	Tokens          int64
	BlobBytesUnique int64
}

// Stats aggregates a small set of metrics-friendly counts in three queries.
// Cheap on shipd-scale catalogs (dozens to thousands of rows); a Prometheus
// scrape every 30s is well under the cost of one publish.
func (s *Store) Stats(ctx context.Context) (StorageStats, error) {
	var st StorageStats
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM apps`).Scan(&st.Apps); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN yanked = 0 THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN yanked = 1 THEN 1 ELSE 0 END), 0)
		FROM releases
	`).Scan(&st.ReleasesLive, &st.ReleasesYanked); err != nil {
		return st, err
	}
	// DISTINCT by blob_key dedups: the same content uploaded under two
	// release rows occupies one blob on disk.
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(size), 0) FROM (
		  SELECT blob_key, MIN(size) AS size FROM releases GROUP BY blob_key
		)
	`).Scan(&st.BlobBytesUnique); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tokens`).Scan(&st.Tokens); err != nil {
		return st, err
	}
	return st, nil
}

// --- Tokens ---

type Token struct {
	Name       string `json:"name"`
	Scope      string `json:"scope"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at,omitempty"` // 0 = never expires
	LastUsedAt int64  `json:"last_used_at"`
}

// ValidScopes lists the scope strings CreateToken (and HTTP / MCP) accept.
// Both shipd auth checks and CLI flag validation reference this set so a
// typo can't slip a token in with an unrecognized scope.
var ValidScopes = map[string]bool{
	"r":     true,
	"rw":    true,
	"admin": true,
}

// CreateToken stores a hashed token. The plaintext value is generated by the
// caller and shown to the user once. expiresAt is the unix-second deadline
// after which LookupToken rejects the token; pass 0 for "never expires".
//
// scope must be one of "r", "rw", or "admin"; an empty string defaults to
// "rw" for back-compat with pre-admin-scope callers.
func (s *Store) CreateToken(ctx context.Context, name, plaintext, scope string, expiresAt int64) error {
	if scope == "" {
		scope = "rw"
	}
	if !ValidScopes[scope] {
		return fmt.Errorf("unknown scope %q (want r, rw, or admin)", scope)
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

// GenerateTokenPlaintext returns a fresh "shipd_..." plaintext token —
// 24 bytes of crypto/rand encoded as URL-safe base64. The same generator is
// used by the local CLI and the HTTP token-creation endpoint so format never
// drifts between paths.
func GenerateTokenPlaintext() (string, error) {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "shipd_" + base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// ParseTTL accepts standard Go duration strings (1h, 30m, 90s) plus the
// security-friendly extensions "Nd" (days) and "Nw" (weeks). Empty input
// returns 0, meaning "never expires" — that's the sentinel both the
// tokens.expires_at column and the install-URL signer treat as "no
// expiration".
//
// Days/weeks aren't part of time.ParseDuration because they're calendar-
// approximate, but for token lifetimes that's fine — operators think in
// "90 days", not "2160h".
func ParseTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if mul, ok := ttlSuffix(s); ok {
		n, err := strconv.Atoi(s[:len(s)-1])
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		if n < 0 {
			return 0, fmt.Errorf("ttl must be non-negative, got %q", s)
		}
		return time.Duration(n) * mul, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("ttl must be non-negative, got %q", s)
	}
	return d, nil
}

func ttlSuffix(s string) (time.Duration, bool) {
	if len(s) < 2 {
		return 0, false
	}
	switch s[len(s)-1] {
	case 'd':
		return 24 * time.Hour, true
	case 'w':
		return 7 * 24 * time.Hour, true
	}
	return 0, false
}

// --- helpers ---

type scanner interface {
	Scan(dest ...any) error
}

func scanRelease(s scanner) (*Release, error) {
	var r Release
	var yanked int
	if err := s.Scan(&r.AppName, &r.Version, &r.Channel, &r.Platform, &r.BlobKey, &r.Size, &r.SHA256,
		&r.Filename, &r.Notes, &yanked, &r.YankedReason, &r.YankedAt,
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
