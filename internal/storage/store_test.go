package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// readSpy wraps an io.Reader and records whether Read was ever called. The
// pre-check in PutRelease is supposed to short-circuit before touching the
// body on a duplicate publish; this lets the test prove it.
type readSpy struct {
	r       io.Reader
	wasRead bool
}

func (s *readSpy) Read(p []byte) (int, error) {
	s.wasRead = true
	return s.r.Read(p)
}

// TestPutReleaseSkipsBlobUploadOnDuplicate is the contract behind the v1.0
// "concurrent-publish dedup" item: republishing the same (app, version,
// channel) must refuse without reading the body, so a CI retry of a
// multi-gigabyte artifact does not pay the upload cost twice.
func TestPutReleaseSkipsBlobUploadOnDuplicate(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.UpsertApp(ctx, "myapp", "ios"); err != nil {
		t.Fatalf("UpsertApp: %v", err)
	}

	if _, err := store.PutRelease(ctx, Release{
		AppName: "myapp", Version: "1.0.0", Channel: "stable",
		Filename: "myapp-1.0.0.ipa",
	}, strings.NewReader("the artifact bytes")); err != nil {
		t.Fatalf("first PutRelease: %v", err)
	}

	spy := &readSpy{r: strings.NewReader("never seen")}
	_, err = store.PutRelease(ctx, Release{
		AppName: "myapp", Version: "1.0.0", Channel: "stable",
		Filename: "myapp-1.0.0.ipa",
	}, spy)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}
	if spy.wasRead {
		t.Error("body was read on duplicate publish; pre-check failed to short-circuit")
	}
}

// TestPutReleaseDefaultsChannelBeforePreCheck guards against a regression
// where channel normalization happens after the pre-check: a publish without
// an explicit channel must still match an existing "stable" release.
func TestPutReleaseDefaultsChannelBeforePreCheck(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.UpsertApp(ctx, "myapp", "ios"); err != nil {
		t.Fatalf("UpsertApp: %v", err)
	}

	if _, err := store.PutRelease(ctx, Release{
		AppName: "myapp", Version: "1.0.0", Channel: "stable",
	}, strings.NewReader("payload")); err != nil {
		t.Fatalf("first PutRelease: %v", err)
	}

	spy := &readSpy{r: strings.NewReader("payload")}
	_, err = store.PutRelease(ctx, Release{
		AppName: "myapp", Version: "1.0.0", Channel: "",
	}, spy)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}
	if spy.wasRead {
		t.Error("body was read on duplicate publish with blank channel")
	}
}

// TestPutReleaseDifferentChannelStillAllowed makes sure the pre-check is
// scoped to (app, version, channel) — a release on `beta` should not block a
// publish of the same version on `stable`.
func TestPutReleaseDifferentChannelStillAllowed(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.UpsertApp(ctx, "myapp", "ios"); err != nil {
		t.Fatalf("UpsertApp: %v", err)
	}

	if _, err := store.PutRelease(ctx, Release{
		AppName: "myapp", Version: "1.0.0", Channel: "beta",
	}, strings.NewReader("payload")); err != nil {
		t.Fatalf("beta PutRelease: %v", err)
	}
	if _, err := store.PutRelease(ctx, Release{
		AppName: "myapp", Version: "1.0.0", Channel: "stable",
	}, strings.NewReader("payload")); err != nil {
		t.Fatalf("stable PutRelease: %v", err)
	}
}
