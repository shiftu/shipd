package server

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testSecret32 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// extractExpSig pulls exp + sig out of a SignedQuery string. SignedQuery
// returns urlencoded values, so we round-trip through url.ParseQuery.
func extractExpSig(t *testing.T, q string) (exp, sig string) {
	t.Helper()
	v, err := url.ParseQuery(q)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", q, err)
	}
	return v.Get("exp"), v.Get("sig")
}

func TestInstallSignerRoundTrip(t *testing.T) {
	s := &installSigner{secret: []byte("test-secret-must-be-32-bytes-min!!"), ttl: time.Minute}
	path := "/install/myapp/1.0.0/download"

	exp, sig := extractExpSig(t, s.SignedQuery(path))
	if exp == "" || sig == "" {
		t.Fatalf("expected exp+sig, got exp=%q sig=%q", exp, sig)
	}
	if err := s.Verify(path, exp, sig); err != nil {
		t.Fatalf("Verify after Sign: %v", err)
	}
}

func TestInstallSignerExpired(t *testing.T) {
	s := &installSigner{secret: []byte("test-secret-must-be-32-bytes-min!!"), ttl: -time.Second}
	path := "/install/myapp/1.0.0/download"

	exp, sig := extractExpSig(t, s.SignedQuery(path))
	err := s.Verify(path, exp, sig)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expected expired error, got %v", err)
	}
}

// TestInstallSignerRefusesReplayAcrossReleases proves the path is part of the
// signed payload — a signature minted for myapp@1.0.0 cannot be replayed
// against otherapp@1.0.0 by swapping the URL prefix.
func TestInstallSignerRefusesReplayAcrossReleases(t *testing.T) {
	s := &installSigner{secret: []byte("test-secret-must-be-32-bytes-min!!"), ttl: time.Minute}
	exp, sig := extractExpSig(t, s.SignedQuery("/install/myapp/1.0.0/download"))

	if err := s.Verify("/install/otherapp/1.0.0/download", exp, sig); err == nil {
		t.Error("expected verify against different app path to fail")
	}
	if err := s.Verify("/install/myapp/2.0.0/download", exp, sig); err == nil {
		t.Error("expected verify against different version path to fail")
	}
}

// TestInstallSignerRefusesTamperedSig: any byte flipped invalidates the MAC.
func TestInstallSignerRefusesTamperedSig(t *testing.T) {
	s := &installSigner{secret: []byte("test-secret-must-be-32-bytes-min!!"), ttl: time.Minute}
	path := "/install/myapp/1.0.0/download"
	exp, sig := extractExpSig(t, s.SignedQuery(path))

	if len(sig) < 4 {
		t.Fatalf("sig too short: %q", sig)
	}
	bad := "AAAA" + sig[4:]
	if err := s.Verify(path, exp, bad); err == nil {
		t.Error("expected mutated sig to fail verify")
	}
}

// TestInstallSignerRefusesTamperedExp: extending the expiration changes the
// signed payload, so the existing sig no longer matches.
func TestInstallSignerRefusesTamperedExp(t *testing.T) {
	s := &installSigner{secret: []byte("test-secret-must-be-32-bytes-min!!"), ttl: time.Minute}
	path := "/install/myapp/1.0.0/download"
	exp, sig := extractExpSig(t, s.SignedQuery(path))

	// Add a year to exp. The signature was minted for the original exp,
	// so this should be rejected.
	expFar := exp + "0" // crude but turns "1714425600" into "17144256000" (~year 2513)
	if err := s.Verify(path, expFar, sig); err == nil {
		t.Error("expected extended exp + original sig to fail verify")
	}
}

func TestLoadOrGenInstallSecretPersistsAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	s1, err := loadOrGenInstallSecret(dir, "")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	s2, err := loadOrGenInstallSecret(dir, "")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if string(s1) != string(s2) {
		t.Error("expected secret to persist across loadOrGen calls; got different values")
	}

	// Verify file mode is 0600 (no group/other read). On macOS the actual
	// mode bits will be exact; we mask before comparing for portability.
	info, err := os.Stat(filepath.Join(dir, "install_url_secret"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("expected 0600, got %#o", info.Mode().Perm())
	}
}

func TestLoadOrGenInstallSecretExplicitOverride(t *testing.T) {
	dir := t.TempDir()
	s, err := loadOrGenInstallSecret(dir, testSecret32)
	if err != nil {
		t.Fatalf("loadOrGen: %v", err)
	}
	if len(s) != 32 {
		t.Errorf("expected 32-byte secret, got %d", len(s))
	}
	// File should NOT be created when an explicit value is given.
	if _, err := os.Stat(filepath.Join(dir, "install_url_secret")); !os.IsNotExist(err) {
		t.Errorf("expected no persisted file, got err=%v", err)
	}
}

func TestLoadOrGenInstallSecretRejectsShort(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrGenInstallSecret(dir, "0123456789ab"); err == nil {
		t.Error("expected short secret to be rejected")
	}
}
