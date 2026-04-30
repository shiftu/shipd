package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
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

// --- PromoteRelease ---

// publishOne is a test helper that creates a release row with the given
// channel and a small body. Returns the inserted release.
func publishOne(t *testing.T, store *Store, app, version, channel, notes string) *Release {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertApp(ctx, app, "ios"); err != nil {
		t.Fatalf("UpsertApp: %v", err)
	}
	rel, err := store.PutRelease(ctx, Release{
		AppName: app, Version: version, Channel: channel, Platform: "ios",
		Filename: app + "-" + version + ".ipa",
		Notes:    notes,
		BundleID: "com.example." + app,
	}, strings.NewReader("payload-"+version+"-"+channel))
	if err != nil {
		t.Fatalf("PutRelease %s@%s [%s]: %v", app, version, channel, err)
	}
	return rel
}

// TestPromoteReleaseHappyPath exercises the staged-rollout flow: publish to
// beta, then promote to stable. The destination must share the source's
// blob_key (no re-upload), inherit notes/bundle_id, and start non-yanked.
func TestPromoteReleaseHappyPath(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	beta := publishOne(t, store, "myapp", "1.0.0", "beta", "first cut")

	stable, err := store.PromoteRelease(ctx, "myapp", "1.0.0", "beta", "stable")
	if err != nil {
		t.Fatalf("PromoteRelease: %v", err)
	}
	if stable.BlobKey != beta.BlobKey {
		t.Errorf("expected shared blob_key %q, got %q", beta.BlobKey, stable.BlobKey)
	}
	if stable.Channel != "stable" {
		t.Errorf("dst.Channel = %q, want stable", stable.Channel)
	}
	if stable.Notes != "first cut" || stable.BundleID != "com.example.myapp" {
		t.Errorf("metadata not carried over: notes=%q bundle_id=%q", stable.Notes, stable.BundleID)
	}
	if stable.Yanked {
		t.Error("dst should start non-yanked")
	}

	// Both rows visible via ListReleases.
	rels, err := store.ListReleases(ctx, "myapp")
	if err != nil {
		t.Fatalf("ListReleases: %v", err)
	}
	if len(rels) != 2 {
		t.Fatalf("expected 2 releases (beta + stable), got %d", len(rels))
	}
}

// TestPromoteReleaseAutoDetectsSingleChannel: when srcChannel is empty and
// the version exists on exactly one channel, that channel is the source.
func TestPromoteReleaseAutoDetectsSingleChannel(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	publishOne(t, store, "myapp", "1.0.0", "beta", "")
	rel, err := store.PromoteRelease(ctx, "myapp", "1.0.0", "", "stable")
	if err != nil {
		t.Fatalf("PromoteRelease (auto-detect): %v", err)
	}
	if rel.Channel != "stable" {
		t.Errorf("dst.Channel = %q, want stable", rel.Channel)
	}
}

// TestPromoteReleaseAmbiguousWithoutFrom: with srcChannel empty and the
// version on multiple channels, the call must refuse and ask for an
// explicit source.
func TestPromoteReleaseAmbiguousWithoutFrom(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	publishOne(t, store, "myapp", "1.0.0", "alpha", "")
	publishOne(t, store, "myapp", "1.0.0", "beta", "")

	_, err = store.PromoteRelease(ctx, "myapp", "1.0.0", "", "stable")
	if err == nil {
		t.Fatal("expected ambiguity error, got nil")
	}
	if !strings.Contains(err.Error(), "multiple channels") {
		t.Errorf("expected error to mention multiple channels, got %v", err)
	}
}

// TestPromoteReleaseSourceNotFound: nothing to promote.
func TestPromoteReleaseSourceNotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	_, err = store.PromoteRelease(ctx, "ghost", "1.0.0", "", "stable")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestPromoteReleaseTargetAlreadyExists: promoting onto a channel that
// already has the same version maps to ErrAlreadyExists, so HTTP returns 409.
func TestPromoteReleaseTargetAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	publishOne(t, store, "myapp", "1.0.0", "beta", "")
	publishOne(t, store, "myapp", "1.0.0", "stable", "")

	_, err = store.PromoteRelease(ctx, "myapp", "1.0.0", "beta", "stable")
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}
}

// TestPromoteReleaseRefusesYankedSource: don't propagate a known-bad build.
func TestPromoteReleaseRefusesYankedSource(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	publishOne(t, store, "myapp", "1.0.0", "beta", "")
	if err := store.YankRelease(ctx, "myapp", "1.0.0", "beta", "broken"); err != nil {
		t.Fatalf("YankRelease: %v", err)
	}

	_, err = store.PromoteRelease(ctx, "myapp", "1.0.0", "beta", "stable")
	if err == nil {
		t.Fatal("expected refusal of yanked source, got nil")
	}
	if !strings.Contains(err.Error(), "yanked") {
		t.Errorf("expected error to mention yanked, got %v", err)
	}
}

// TestPromoteReleaseRefusesSameChannel: src == dst is a no-op at best, more
// likely a typo.
func TestPromoteReleaseRefusesSameChannel(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	publishOne(t, store, "myapp", "1.0.0", "stable", "")

	_, err = store.PromoteRelease(ctx, "myapp", "1.0.0", "stable", "stable")
	if err == nil {
		t.Fatal("expected refusal of same-channel promotion")
	}
}

// --- LatestReleases ---

// putReleaseForPlatform creates a release on a specific platform without the
// publishOne helper's defaults. Used by the multi-platform tests where the
// platform value is the whole point.
func putReleaseForPlatform(t *testing.T, store *Store, app, version, channel, platform string) {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertApp(ctx, app, platform); err != nil {
		t.Fatalf("UpsertApp: %v", err)
	}
	if _, err := store.PutRelease(ctx, Release{
		AppName: app, Version: version, Channel: channel, Platform: platform,
		Filename: app + "-" + version + "." + platform,
		BundleID: "com.example." + app,
	}, strings.NewReader("payload-"+version+"-"+platform)); err != nil {
		t.Fatalf("PutRelease %s@%s [%s]: %v", app, version, platform, err)
	}
}

// TestLatestReleasesOnePerPlatform reproduces the install-page bug: an iOS
// upload followed by an Android upload used to make /install/{app} show only
// the Android one. LatestReleases must surface both.
func TestLatestReleasesOnePerPlatform(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	putReleaseForPlatform(t, store, "zendiac", "1.0.0", "stable", "ios")
	putReleaseForPlatform(t, store, "zendiac", "1.0.1", "stable", "android")

	rels, err := store.LatestReleases(ctx, "zendiac", "")
	if err != nil {
		t.Fatalf("LatestReleases: %v", err)
	}
	if len(rels) != 2 {
		t.Fatalf("expected 2 (one per platform), got %d", len(rels))
	}
	// Sorted alphabetically: android, ios.
	if rels[0].Platform != "android" || rels[1].Platform != "ios" {
		t.Errorf("got platforms [%s, %s], want [android, ios]", rels[0].Platform, rels[1].Platform)
	}
	if rels[0].Version != "1.0.1" || rels[1].Version != "1.0.0" {
		t.Errorf("got versions [%s, %s], want [1.0.1, 1.0.0]", rels[0].Version, rels[1].Version)
	}
}

// TestLatestReleasesPicksNewestPerPlatform: with multiple iOS releases over
// time, LatestReleases must pick the newest, not the first/oldest.
func TestLatestReleasesPicksNewestPerPlatform(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	putReleaseForPlatform(t, store, "myapp", "1.0.0", "stable", "ios")
	putReleaseForPlatform(t, store, "myapp", "1.1.0", "stable", "ios")
	putReleaseForPlatform(t, store, "myapp", "2.0.0", "stable", "ios")

	rels, err := store.LatestReleases(ctx, "myapp", "")
	if err != nil {
		t.Fatalf("LatestReleases: %v", err)
	}
	if len(rels) != 1 || rels[0].Version != "2.0.0" {
		t.Errorf("expected single newest iOS release 2.0.0, got %+v", rels)
	}
}

// TestLatestReleasesIgnoresYanked: a yanked release on one platform must not
// appear, but it shouldn't disqualify other non-yanked releases on the same
// platform either.
func TestLatestReleasesIgnoresYanked(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	putReleaseForPlatform(t, store, "myapp", "1.0.0", "stable", "ios")
	putReleaseForPlatform(t, store, "myapp", "1.0.1", "stable", "ios")
	if err := store.YankRelease(ctx, "myapp", "1.0.1", "stable", "broken"); err != nil {
		t.Fatalf("YankRelease: %v", err)
	}

	rels, err := store.LatestReleases(ctx, "myapp", "")
	if err != nil {
		t.Fatalf("LatestReleases: %v", err)
	}
	if len(rels) != 1 || rels[0].Version != "1.0.0" {
		t.Errorf("expected fall-back to non-yanked 1.0.0, got %+v", rels)
	}
}

// TestLatestReleasesEmptyApp: nothing published → empty slice, no error.
func TestLatestReleasesEmptyApp(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	rels, err := store.LatestReleases(context.Background(), "ghost", "")
	if err != nil {
		t.Fatalf("LatestReleases: %v", err)
	}
	if len(rels) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(rels))
	}
}

// --- Token expiry ---

// TestTokenNeverExpiresWithZeroExpiry guards the back-compat default: a
// token created with expiresAt=0 must keep working forever.
func TestTokenNeverExpiresWithZeroExpiry(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.CreateToken(ctx, "forever", "shipd_forever", "rw", 0); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	tok, err := store.LookupToken(ctx, "shipd_forever")
	if err != nil {
		t.Fatalf("LookupToken: %v", err)
	}
	if tok.ExpiresAt != 0 {
		t.Errorf("ExpiresAt = %d, want 0", tok.ExpiresAt)
	}
}

// TestTokenWithFutureExpiryLooksUp: a token good for an hour is accepted now.
func TestTokenWithFutureExpiryLooksUp(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	exp := time.Now().Add(time.Hour).Unix()
	if err := store.CreateToken(ctx, "hourly", "shipd_hourly", "rw", exp); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	tok, err := store.LookupToken(ctx, "shipd_hourly")
	if err != nil {
		t.Fatalf("LookupToken: %v", err)
	}
	if tok.ExpiresAt != exp {
		t.Errorf("ExpiresAt = %d, want %d", tok.ExpiresAt, exp)
	}
}

// TestTokenExpiredReturnsErrExpired is the security contract: an expired
// token gets ErrExpired (which the HTTP layer maps to 401), and last_used_at
// must NOT be refreshed by the failed lookup.
func TestTokenExpiredReturnsErrExpired(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	pastExp := time.Now().Add(-time.Second).Unix()
	if err := store.CreateToken(ctx, "stale", "shipd_stale", "rw", pastExp); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	_, err = store.LookupToken(ctx, "shipd_stale")
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("expected ErrExpired, got %v", err)
	}

	// The token still appears in ListTokens (so an operator can see and
	// revoke it). last_used_at should be 0 — the failed lookup must not
	// have updated it.
	toks, err := store.ListTokens(ctx)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	var found *Token
	for i := range toks {
		if toks[i].Name == "stale" {
			found = &toks[i]
			break
		}
	}
	if found == nil {
		t.Fatal("expired token missing from ListTokens")
	}
	if found.LastUsedAt != 0 {
		t.Errorf("LastUsedAt = %d, want 0 (expired lookup must not refresh it)", found.LastUsedAt)
	}
}
