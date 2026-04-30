package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// installSigner mints and verifies HMAC-signed query params for the public
// install routes (download + plist). The secret is loaded once at startup
// and held in memory.
//
// Scheme: HMAC-SHA256(secret, "<path>:<exp_unix>"), digest base64url-encoded
// without padding. Attached to the URL as ?exp=<unix>&sig=<digest>.
//
// The path includes the app + version, so a signature minted for one release
// cannot be replayed against another. The expiration is part of the signed
// payload, so a client tampering with ?exp= invalidates the signature.
type installSigner struct {
	secret []byte
	ttl    time.Duration
}

// SignedQuery returns "exp=...&sig=..." (urlencoded) ready to append after
// "?". The TTL is the signer's configured value.
func (s *installSigner) SignedQuery(path string) string {
	exp := time.Now().Add(s.ttl).Unix()
	sig := s.compute(path, exp)
	q := url.Values{}
	q.Set("exp", strconv.FormatInt(exp, 10))
	q.Set("sig", sig)
	return q.Encode()
}

// Verify returns nil iff (path, exp, sig) is a current, untampered signature.
func (s *installSigner) Verify(path, expStr, sig string) error {
	if expStr == "" || sig == "" {
		return errors.New("missing signature")
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return errors.New("invalid expiration")
	}
	if time.Now().Unix() > exp {
		return errors.New("link expired")
	}
	want := s.compute(path, exp)
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return errors.New("invalid signature")
	}
	return nil
}

func (s *installSigner) compute(path string, exp int64) string {
	h := hmac.New(sha256.New, s.secret)
	h.Write([]byte(path))
	h.Write([]byte{':'})
	h.Write([]byte(strconv.FormatInt(exp, 10)))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// loadOrGenInstallSecret resolves the install-URL signing secret:
//
//   - if explicit is non-empty, decode it as hex and use it as-is
//     (must be ≥ 32 bytes).
//   - else if <dataDir>/install_url_secret exists, read and decode it.
//   - else generate a fresh 32-byte secret, persist it to that file (mode
//     0600), and return it. Subsequent restarts find the file and reuse it
//     so signed URLs that are still within TTL survive a server restart.
//
// Operators running multiple shipd replicas should set the explicit value so
// every replica signs with the same key.
func loadOrGenInstallSecret(dataDir, explicit string) ([]byte, error) {
	if explicit != "" {
		b, err := hex.DecodeString(explicit)
		if err != nil {
			return nil, fmt.Errorf("install-url-secret: hex decode: %w", err)
		}
		if len(b) < 32 {
			return nil, errors.New("install-url-secret: must be at least 32 bytes (64 hex chars)")
		}
		return b, nil
	}

	path := filepath.Join(dataDir, "install_url_secret")
	if data, err := os.ReadFile(path); err == nil {
		if b, err := hex.DecodeString(string(data)); err == nil && len(b) >= 32 {
			return b, nil
		}
		// Corrupt or short — fall through and regenerate.
	}

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("install-url-secret: random: %w", err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(secret)), 0o600); err != nil {
		return nil, fmt.Errorf("install-url-secret: persist: %w", err)
	}
	return secret, nil
}
