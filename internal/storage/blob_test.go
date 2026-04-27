package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestFSBlobStoreRoundTrip exercises the filesystem backend end-to-end:
// content addressing, dedup of identical bytes, and Get-after-Put recovery.
func TestFSBlobStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFSBlobStore(dir)
	if err != nil {
		t.Fatalf("NewFSBlobStore: %v", err)
	}
	ctx := context.Background()
	payload := []byte("hello shipd")
	wantSum := sha256Hex(payload)

	key, size, sum, err := store.Put(ctx, strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if key != wantSum || sum != wantSum {
		t.Errorf("expected key/sum=%s, got key=%s sum=%s", wantSum, key, sum)
	}
	if size != int64(len(payload)) {
		t.Errorf("size=%d want %d", size, len(payload))
	}

	rc, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("round-trip mismatch: %q vs %q", got, payload)
	}

	// Dedup: putting identical content yields the same key without error.
	key2, _, _, err := store.Put(ctx, strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("Put (dedup): %v", err)
	}
	if key2 != key {
		t.Errorf("expected dedup to same key, got %s vs %s", key2, key)
	}
}

// fakeS3 is a minimal in-process server that speaks just enough of the S3
// protocol for our Put/Head/Get path. It's not a full S3 emulator — calls
// outside that surface 501 to flag accidental dependency on more endpoints.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func (f *fakeS3) Server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.objects[key] = body
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			f.mu.Lock()
			_, ok := f.objects[key]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			f.mu.Lock()
			body, ok := f.objects[key]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(body)
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
}

// TestS3BlobStoreRoundTripAgainstFake verifies the S3 backend against a
// fake S3 endpoint: keys are correctly composed, dedup short-circuits on
// HeadObject, and the bytes round-trip.
func TestS3BlobStoreRoundTripAgainstFake(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	fake := newFakeS3()
	srv := fake.Server()
	defer srv.Close()

	ctx := context.Background()
	store, err := NewS3BlobStore(ctx, S3Config{
		Bucket:    "test-bucket",
		Region:    "us-east-1",
		Endpoint:  srv.URL,
		Prefix:    "blobs/",
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3BlobStore: %v", err)
	}

	payload := []byte("ship it")
	wantSum := sha256Hex(payload)

	key, size, sum, err := store.Put(ctx, strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if key != wantSum || sum != wantSum {
		t.Errorf("expected key=%s, got %s", wantSum, key)
	}
	if size != int64(len(payload)) {
		t.Errorf("size=%d want %d", size, len(payload))
	}

	// The composed S3 key should be "test-bucket/blobs/<sha[:2]>/<sha[2:]>"
	// in path-style. Verify by hitting the fake directly.
	wantPath := "test-bucket/blobs/" + wantSum[:2] + "/" + wantSum[2:]
	fake.mu.Lock()
	got, ok := fake.objects[wantPath]
	fake.mu.Unlock()
	if !ok {
		t.Fatalf("object not stored at expected path %q (have %v)", wantPath, mapKeys(fake.objects))
	}
	if string(got) != string(payload) {
		t.Errorf("stored bytes differ from payload")
	}

	// Get returns the same bytes.
	rc, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	roundTrip, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(roundTrip) != string(payload) {
		t.Errorf("round-trip mismatch: %q vs %q", roundTrip, payload)
	}

	// Dedup: a second Put with identical content should NOT reach PutObject —
	// HeadObject finds it and short-circuits. We verify by mutating the
	// stored object and checking that the second Put doesn't overwrite.
	fake.mu.Lock()
	fake.objects[wantPath] = []byte("tampered")
	fake.mu.Unlock()
	if _, _, _, err := store.Put(ctx, strings.NewReader(string(payload))); err != nil {
		t.Fatalf("Put (dedup): %v", err)
	}
	fake.mu.Lock()
	current := string(fake.objects[wantPath])
	fake.mu.Unlock()
	if current != "tampered" {
		t.Errorf("expected dedup to skip upload, but object was overwritten to %q", current)
	}
}

// TestS3GetMissing maps an S3 NotFound to the package's ErrNotFound.
func TestS3GetMissing(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	srv := newFakeS3().Server()
	defer srv.Close()

	store, err := NewS3BlobStore(context.Background(), S3Config{
		Bucket:    "test-bucket",
		Region:    "us-east-1",
		Endpoint:  srv.URL,
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3BlobStore: %v", err)
	}
	if _, err := store.Get(context.Background(), "deadbeef"); err == nil {
		t.Error("expected error for missing key")
	} else if err != ErrNotFound {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func mapKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
