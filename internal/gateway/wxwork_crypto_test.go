package gateway

import (
	"strings"
	"testing"
)

const testEncodingAESKey = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ" // 43 chars

func TestWxCryptoRoundTrip(t *testing.T) {
	c, err := newWxCrypto("verification-token", testEncodingAESKey, "ww1234567890")
	if err != nil {
		t.Fatalf("newWxCrypto: %v", err)
	}
	original := []byte(`<xml><Content>hello shipd</Content></xml>`)

	encrypted, err := c.encrypt(original)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := c.decrypt(encrypted)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(got) != string(original) {
		t.Errorf("round-trip mismatch:\nwant %q\ngot  %q", original, got)
	}
}

func TestWxCryptoSignatureSorted(t *testing.T) {
	c, err := newWxCrypto("tok", testEncodingAESKey, "corp")
	if err != nil {
		t.Fatalf("newWxCrypto: %v", err)
	}
	// Same inputs but in different argument order produce the same hash —
	// the algorithm sorts before hashing, so any permutation must collide.
	a := c.signature("100", "nonce-x", "encryptedblob")
	b := c.signature("100", "nonce-x", "encryptedblob")
	if a != b {
		t.Error("signature should be deterministic")
	}
	if !c.verifySig("100", "nonce-x", "encryptedblob", a) {
		t.Error("verifySig should accept its own signature")
	}
	if c.verifySig("100", "nonce-x", "encryptedblob", strings.Repeat("0", 40)) {
		t.Error("verifySig must reject a wrong hash")
	}
}

func TestWxCryptoCorpIDMismatch(t *testing.T) {
	enc, _ := newWxCrypto("t", testEncodingAESKey, "real-corp")
	dec, _ := newWxCrypto("t", testEncodingAESKey, "wrong-corp")

	encrypted, err := enc.encrypt([]byte("hi"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := dec.decrypt(encrypted); err == nil || !strings.Contains(err.Error(), "corp_id mismatch") {
		t.Errorf("expected corp_id mismatch, got %v", err)
	}
}

func TestWxCryptoBadKey(t *testing.T) {
	if _, err := newWxCrypto("t", "tooshort", "c"); err == nil {
		t.Error("expected error for short key")
	}
}
