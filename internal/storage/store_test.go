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

// --- GC ---

// TestYankRecordsYankedAt: yanking a release stamps the time, so gc can
// later filter by --older-than.
func TestYankRecordsYankedAt(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	publishOne(t, store, "myapp", "1.0.0", "stable", "")
	before := time.Now().Unix()
	if err := store.YankRelease(ctx, "myapp", "1.0.0", "stable", "broken"); err != nil {
		t.Fatalf("YankRelease: %v", err)
	}
	after := time.Now().Unix()

	rel, err := store.GetRelease(ctx, "myapp", "1.0.0", "stable")
	if err != nil {
		t.Fatalf("GetRelease: %v", err)
	}
	if rel.YankedAt < before || rel.YankedAt > after {
		t.Errorf("YankedAt = %d, expected within [%d, %d]", rel.YankedAt, before, after)
	}
}

// TestGCCandidatesFiltersByYankAge: only releases yanked more than the
// olderThan window ago are returned. Recently-yanked releases must NOT
// appear, since they may still be needed (yank reasons sometimes get
// reversed within hours).
func TestGCCandidatesFiltersByYankAge(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	publishOne(t, store, "myapp", "1.0.0", "stable", "")
	publishOne(t, store, "myapp", "1.0.1", "stable", "")
	if err := store.YankRelease(ctx, "myapp", "1.0.0", "stable", ""); err != nil {
		t.Fatalf("yank 1.0.0: %v", err)
	}
	if err := store.YankRelease(ctx, "myapp", "1.0.1", "stable", ""); err != nil {
		t.Fatalf("yank 1.0.1: %v", err)
	}
	// Backdate 1.0.0 to ~60 days ago, leave 1.0.1 just-yanked.
	old := time.Now().Add(-60 * 24 * time.Hour).Unix()
	if _, err := store.db.ExecContext(ctx,
		`UPDATE releases SET yanked_at = ? WHERE version = '1.0.0'`, old); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// keepLast=0 disables the safety net so this test focuses purely on
	// the age filter; keepLast behavior is exercised in TestGCCandidatesKeepLast.
	got30d, err := store.GCCandidates(ctx, 30*24*time.Hour, 0)
	if err != nil {
		t.Fatalf("GCCandidates 30d: %v", err)
	}
	if len(got30d) != 1 || got30d[0].Version != "1.0.0" {
		t.Errorf("with 30d window expected only 1.0.0, got %v", versionList(got30d))
	}

	got0, err := store.GCCandidates(ctx, 0, 0)
	if err != nil {
		t.Fatalf("GCCandidates 0: %v", err)
	}
	if len(got0) != 2 {
		t.Errorf("with 0 window expected both, got %v", versionList(got0))
	}
}

// TestGCCandidatesIncludesPreMigrationYanks: rows yanked before the
// yanked_at column existed have yanked_at=0, which sorts as "the dawn of
// time" and must always be eligible. Operators upgrading shipd shouldn't
// have to manually patch old yanks to be GC-able.
func TestGCCandidatesIncludesPreMigrationYanks(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	publishOne(t, store, "myapp", "1.0.0", "stable", "")
	if err := store.YankRelease(ctx, "myapp", "1.0.0", "stable", ""); err != nil {
		t.Fatalf("yank: %v", err)
	}
	// Simulate a pre-migration yank: yanked=1 but yanked_at=0.
	if _, err := store.db.ExecContext(ctx,
		`UPDATE releases SET yanked_at = 0 WHERE version = '1.0.0'`); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	got, err := store.GCCandidates(ctx, 30*24*time.Hour, 0)
	if err != nil {
		t.Fatalf("GCCandidates: %v", err)
	}
	if len(got) != 1 || got[0].YankedAt != 0 {
		t.Errorf("expected pre-migration yank to be eligible, got %+v", got)
	}
}

// TestGCCandidatesIgnoresLiveReleases: only yanked rows are candidates;
// non-yanked releases must never appear, no matter how old.
func TestGCCandidatesIgnoresLiveReleases(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	publishOne(t, store, "myapp", "1.0.0", "stable", "")
	got, err := store.GCCandidates(ctx, 0, 0)
	if err != nil {
		t.Fatalf("GCCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no candidates among live releases, got %v", versionList(got))
	}
}

// TestGCCandidatesKeepLast: even with all releases yanked-and-old, the N
// most-recently-published per (app, channel, platform) must be protected.
// This is the safety net for slow-moving apps where yanks accumulate over
// time and would otherwise eat the entire history.
func TestGCCandidatesKeepLast(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	// Five iOS releases, all yanked. Backdate yanks to 60d so the age
	// filter doesn't shadow the keepLast effect.
	for _, v := range []string{"1.0.0", "1.0.1", "1.0.2", "1.0.3", "1.0.4"} {
		publishOne(t, store, "myapp", v, "stable", "")
		if err := store.YankRelease(ctx, "myapp", v, "stable", ""); err != nil {
			t.Fatalf("yank %s: %v", v, err)
		}
	}
	old := time.Now().Add(-60 * 24 * time.Hour).Unix()
	if _, err := store.db.ExecContext(ctx, `UPDATE releases SET yanked_at = ?`, old); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	cases := []struct {
		keepLast    int
		wantCandLen int
		// Versions that must NOT appear in the candidate list (i.e., kept).
		// publishOne inserts in order, so 1.0.4 is the most-recent.
		mustKeep []string
	}{
		{keepLast: 0, wantCandLen: 5, mustKeep: nil},
		{keepLast: 1, wantCandLen: 4, mustKeep: []string{"1.0.4"}},
		{keepLast: 2, wantCandLen: 3, mustKeep: []string{"1.0.3", "1.0.4"}},
		{keepLast: 5, wantCandLen: 0, mustKeep: []string{"1.0.0", "1.0.1", "1.0.2", "1.0.3", "1.0.4"}},
		{keepLast: 100, wantCandLen: 0, mustKeep: []string{"1.0.0", "1.0.1", "1.0.2", "1.0.3", "1.0.4"}},
	}
	for _, c := range cases {
		got, err := store.GCCandidates(ctx, 0, c.keepLast)
		if err != nil {
			t.Fatalf("GCCandidates keep=%d: %v", c.keepLast, err)
		}
		if len(got) != c.wantCandLen {
			t.Errorf("keep=%d: got %d candidates %v, want %d", c.keepLast, len(got), versionList(got), c.wantCandLen)
		}
		seen := map[string]bool{}
		for _, r := range got {
			seen[r.Version] = true
		}
		for _, must := range c.mustKeep {
			if seen[must] {
				t.Errorf("keep=%d: protected version %s appeared as candidate", c.keepLast, must)
			}
		}
	}
}

// TestGCCandidatesKeepLastPerPlatform: the partition includes platform, so
// an iOS+Android app keeps one of each platform under --keep-last 1, not
// "one of either platform whichever was newest overall".
func TestGCCandidatesKeepLastPerPlatform(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	putReleaseForPlatform(t, store, "zendiac", "1.0.0", "stable", "ios")
	putReleaseForPlatform(t, store, "zendiac", "1.0.1", "stable", "ios")
	putReleaseForPlatform(t, store, "zendiac", "2.0.0", "stable", "android")
	for _, v := range []string{"1.0.0", "1.0.1", "2.0.0"} {
		if err := store.YankRelease(ctx, "zendiac", v, "stable", ""); err != nil {
			t.Fatalf("yank %s: %v", v, err)
		}
	}
	old := time.Now().Add(-60 * 24 * time.Hour).Unix()
	if _, err := store.db.ExecContext(ctx, `UPDATE releases SET yanked_at = ?`, old); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	got, err := store.GCCandidates(ctx, 0, 1)
	if err != nil {
		t.Fatalf("GCCandidates: %v", err)
	}
	// Should keep 1.0.1 (newest iOS) and 2.0.0 (only Android). Only 1.0.0
	// remains as a candidate.
	if len(got) != 1 || got[0].Version != "1.0.0" {
		t.Errorf("expected only [1.0.0] as candidate, got %v", versionList(got))
	}
}

// TestDeleteReleaseAndBlobHappyPath: row gone from SQLite, blob gone from
// storage.
func TestDeleteReleaseAndBlobHappyPath(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	rel := publishOne(t, store, "myapp", "1.0.0", "stable", "")
	blobDeleted, err := store.DeleteReleaseAndBlob(ctx, "myapp", "1.0.0", "stable")
	if err != nil {
		t.Fatalf("DeleteReleaseAndBlob: %v", err)
	}
	if !blobDeleted {
		t.Error("expected blobDeleted=true (no other release shares this blob)")
	}
	if _, err := store.GetRelease(ctx, "myapp", "1.0.0", "stable"); err == nil {
		t.Error("expected GetRelease to return ErrNotFound after delete")
	}
	if _, err := store.blobs.Get(ctx, rel.BlobKey); err == nil {
		t.Error("expected blob to be gone after delete")
	}
}

// TestDeleteReleaseAndBlobKeepsSharedBlob is the dedup-safety case. When
// two releases share a content-addressed blob (because their bytes are
// identical), deleting one must leave the blob in place for the other.
// Bytes are the same when both PutRelease calls hand the same content to
// stagedBlob — easy to force in a test.
func TestDeleteReleaseAndBlobKeepsSharedBlob(t *testing.T) {
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
	relA, err := store.PutRelease(ctx, Release{
		AppName: "myapp", Version: "1.0.0", Channel: "beta",
		Filename: "a.ipa",
	}, strings.NewReader("identical-bytes"))
	if err != nil {
		t.Fatalf("PutRelease A: %v", err)
	}
	relB, err := store.PutRelease(ctx, Release{
		AppName: "myapp", Version: "1.0.0", Channel: "stable",
		Filename: "b.ipa",
	}, strings.NewReader("identical-bytes"))
	if err != nil {
		t.Fatalf("PutRelease B: %v", err)
	}
	if relA.BlobKey != relB.BlobKey {
		t.Fatalf("expected dedup; got distinct keys %s / %s", relA.BlobKey, relB.BlobKey)
	}

	blobDeleted, err := store.DeleteReleaseAndBlob(ctx, "myapp", "1.0.0", "beta")
	if err != nil {
		t.Fatalf("DeleteReleaseAndBlob: %v", err)
	}
	if blobDeleted {
		t.Error("expected blobDeleted=false (other release still references the blob)")
	}
	// stable row still resolvable, blob still readable through it.
	stable, err := store.GetRelease(ctx, "myapp", "1.0.0", "stable")
	if err != nil {
		t.Fatalf("GetRelease stable: %v", err)
	}
	rc, err := store.OpenBlob(stable)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	rc.Close()
}

// TestDeleteReleaseAndBlobMissing: deleting a non-existent release returns
// ErrNotFound rather than silently succeeding.
func TestDeleteReleaseAndBlobMissing(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	_, err = store.DeleteReleaseAndBlob(context.Background(), "ghost", "1.0.0", "stable")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func versionList(rels []Release) []string {
	out := make([]string, len(rels))
	for i, r := range rels {
		out[i] = r.Version
	}
	return out
}

// --- Unyank ---

// TestUnyankReverses: yank a release, then unyank it. yanked / yanked_at /
// yanked_reason all reset; subsequent LatestRelease picks it up again.
func TestUnyankReverses(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	publishOne(t, store, "myapp", "1.0.0", "stable", "")
	if err := store.YankRelease(ctx, "myapp", "1.0.0", "stable", "broken"); err != nil {
		t.Fatalf("YankRelease: %v", err)
	}
	if err := store.UnyankRelease(ctx, "myapp", "1.0.0", "stable"); err != nil {
		t.Fatalf("UnyankRelease: %v", err)
	}
	rel, err := store.GetRelease(ctx, "myapp", "1.0.0", "stable")
	if err != nil {
		t.Fatalf("GetRelease: %v", err)
	}
	if rel.Yanked || rel.YankedReason != "" || rel.YankedAt != 0 {
		t.Errorf("expected fully reset, got yanked=%v reason=%q at=%d",
			rel.Yanked, rel.YankedReason, rel.YankedAt)
	}
	// LatestRelease (which filters yanked) should now find it.
	if _, err := store.LatestRelease(ctx, "myapp", "stable"); err != nil {
		t.Errorf("LatestRelease should find unyanked release: %v", err)
	}
}

// TestUnyankIdempotent: unyanking a not-currently-yanked release is fine.
// The recovery path may be invoked by an automation that doesn't know the
// current state.
func TestUnyankIdempotent(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	publishOne(t, store, "myapp", "1.0.0", "stable", "")
	if err := store.UnyankRelease(ctx, "myapp", "1.0.0", "stable"); err != nil {
		t.Errorf("unyank on non-yanked release should be idempotent, got %v", err)
	}
}

// TestUnyankMissingReleaseReturnsErrNotFound: distinguishes "row exists but
// wasn't yanked" (idempotent success) from "row doesn't exist" (404).
func TestUnyankMissingReleaseReturnsErrNotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	err = store.UnyankRelease(context.Background(), "ghost", "1.0.0", "stable")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// --- Token scope validation ---

// TestCreateTokenRejectsInvalidScope: only r/rw/admin accepted; typos like
// "write" fail loudly so a token can't sneak in with an unrecognized scope
// that the auth middleware would treat as zero-rank (i.e., always denied).
func TestCreateTokenRejectsInvalidScope(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	for _, bad := range []string{"write", "ADMIN", "rw ", "rwx", "owner"} {
		if err := store.CreateToken(ctx, bad, "shipd_x_"+bad, bad, 0); err == nil {
			t.Errorf("expected CreateToken to reject scope %q, got nil", bad)
		}
	}
	for _, good := range []string{"r", "rw", "admin"} {
		if err := store.CreateToken(ctx, "ok-"+good, "shipd_x_"+good, good, 0); err != nil {
			t.Errorf("expected CreateToken to accept scope %q, got %v", good, err)
		}
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

// --- Stats ---

// TestStatsAggregates: Stats counts apps/releases/tokens and dedups blob
// bytes by blob_key. The dedup matters for /metrics — referenced bytes
// would double-count when promote creates a second row pointing at the
// same content.
func TestStatsAggregates(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	// Two apps; three releases on app-a (one yanked); one release on app-b
	// promoted to a second channel (same blob_key — dedup).
	publishOne(t, store, "app-a", "1.0.0", "stable", "")
	publishOne(t, store, "app-a", "1.0.1", "stable", "")
	rel := publishOne(t, store, "app-a", "1.0.2", "stable", "")
	if err := store.YankRelease(ctx, "app-a", "1.0.0", "stable", ""); err != nil {
		t.Fatalf("yank: %v", err)
	}
	if err := store.UpsertApp(ctx, "app-b", "ios"); err != nil {
		t.Fatalf("UpsertApp: %v", err)
	}
	relB, err := store.PutRelease(ctx, Release{
		AppName: "app-b", Version: "1.0.0", Channel: "beta",
	}, strings.NewReader("dedup-target"))
	if err != nil {
		t.Fatalf("PutRelease b/beta: %v", err)
	}
	// Promote to stable — identical bytes reuse blob_key.
	if _, err := store.PromoteRelease(ctx, "app-b", "1.0.0", "beta", "stable"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if err := store.CreateToken(ctx, "ci", "shipd_test_ci", "rw", 0); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	s, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.Apps != 2 {
		t.Errorf("Apps = %d, want 2", s.Apps)
	}
	if s.ReleasesLive != 4 || s.ReleasesYanked != 1 {
		t.Errorf("ReleasesLive=%d ReleasesYanked=%d, want 4/1", s.ReleasesLive, s.ReleasesYanked)
	}
	if s.Tokens != 1 {
		t.Errorf("Tokens = %d, want 1", s.Tokens)
	}
	// app-a has three distinct payloads (publishOne uses unique bodies);
	// app-b's two release rows share one blob_key. Total unique blobs = 4.
	want := rel.Size*3 + relB.Size
	if s.BlobBytesUnique != want {
		t.Errorf("BlobBytesUnique = %d, want %d (dedup applied to app-b)", s.BlobBytesUnique, want)
	}
}

// --- ParseTTL ---

// TestParseTTL covers the duration shapes both shipd's CLI flags
// (--ttl, --older-than) and the HTTP admin endpoints accept. The "d" /
// "w" extensions matter most — Go's time.ParseDuration tops out at hours,
// but token lifetimes and gc windows are normally measured in days.
func TestParseTTL(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{in: "", want: 0},
		{in: "0", want: 0}, // Go's ParseDuration accepts bare "0"; same as empty.
		{in: "30m", want: 30 * time.Minute},
		{in: "1h", want: time.Hour},
		{in: "2160h", want: 2160 * time.Hour},
		{in: "1d", want: 24 * time.Hour},
		{in: "90d", want: 90 * 24 * time.Hour},
		{in: "1w", want: 7 * 24 * time.Hour},
		{in: "4w", want: 4 * 7 * 24 * time.Hour},
		{in: "  90d  ", want: 90 * 24 * time.Hour},
		{in: "-1h", err: true},
		{in: "-7d", err: true},
		{in: "abc", err: true},
		{in: "10x", err: true},
	}
	for _, c := range cases {
		got, err := ParseTTL(c.in)
		if c.err {
			if err == nil {
				t.Errorf("ParseTTL(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTTL(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseTTL(%q) = %v, want %v", c.in, got, c.want)
		}
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
