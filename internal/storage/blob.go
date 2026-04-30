package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// BlobStore is the abstraction shipd uses for the actual artifact bytes.
// Two implementations live in this package today (filesystem + S3); more
// can be added without touching the rest of the code as long as they
// honour the content-addressing contract:
//
//   - Put streams body, computes its SHA-256, and stores the bytes under a
//     key derived from that hash. The same bytes uploaded twice MUST land
//     at the same key (so dedup is automatic).
//   - Get retrieves the bytes for a previously-Put key.
//
// Keys are opaque to callers; only Put returns them, only Get consumes them.
type BlobStore interface {
	// Put streams body and returns the content-addressed key, the byte size,
	// and the lowercase hex SHA-256 of the content.
	Put(ctx context.Context, body io.Reader) (key string, size int64, sha256 string, err error)

	// Get returns a reader over the bytes stored under key. The caller must
	// close the reader.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Delete removes the bytes stored under key. Idempotent: deleting a
	// missing key is not an error (S3 returns 204; the FS backend swallows
	// os.IsNotExist). Used by `shipd gc` to reclaim storage from yanked
	// releases.
	Delete(ctx context.Context, key string) error
}

// stagedBlob streams body through a SHA-256 hasher into a temp file, returning
// the open temp file (rewound to 0), the hex digest, and the byte count.
//
// Both backends use this: the filesystem backend renames the temp into place,
// and the S3 backend uploads it. Centralizing the hashing+staging avoids
// duplication and keeps the content-addressing rule in one spot.
//
// The caller owns the returned *os.File and is responsible for closing it
// AND removing it from disk (use cleanup() returned alongside).
func stagedBlob(body io.Reader, dir string) (f *os.File, sum string, size int64, cleanup func(), err error) {
	tmp, err := os.CreateTemp(dir, "blob-*")
	if err != nil {
		return nil, "", 0, nil, fmt.Errorf("create temp: %w", err)
	}
	cleanup = func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), body)
	if err != nil {
		cleanup()
		return nil, "", 0, nil, fmt.Errorf("stage blob: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, "", 0, nil, fmt.Errorf("rewind: %w", err)
	}
	return tmp, hex.EncodeToString(h.Sum(nil)), n, cleanup, nil
}

// --- filesystem implementation ---

// FSBlobStore persists blobs as files under a content-addressed directory
// tree: <root>/<sha[:2]>/<sha[2:]>. The two-character prefix prevents the
// "millions of files in one directory" foot-gun on older filesystems.
type FSBlobStore struct {
	root string
}

func NewFSBlobStore(root string) (*FSBlobStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir blobs: %w", err)
	}
	return &FSBlobStore{root: root}, nil
}

func (s *FSBlobStore) Put(_ context.Context, body io.Reader) (string, int64, string, error) {
	tmp, sum, size, cleanup, err := stagedBlob(body, s.root)
	if err != nil {
		return "", 0, "", err
	}
	defer cleanup()

	dst := s.path(sum)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, "", err
	}
	// os.Rename works because tmp lives under s.root (same filesystem). If a
	// blob with the same hash already exists, the rename atomically replaces
	// it — which is fine, since identical content is identical content.
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", 0, "", fmt.Errorf("rename blob: %w", err)
	}
	return sum, size, sum, nil
}

func (s *FSBlobStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	return os.Open(s.path(key))
}

func (s *FSBlobStore) Delete(_ context.Context, key string) error {
	if err := os.Remove(s.path(key)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *FSBlobStore) path(key string) string {
	if len(key) < 2 {
		return filepath.Join(s.root, key)
	}
	return filepath.Join(s.root, key[:2], key[2:])
}
