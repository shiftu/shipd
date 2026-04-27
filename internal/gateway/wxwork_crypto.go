package gateway

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// wxCrypto bundles the AES key + signature token for a single WeChat Work
// app. Its methods implement Tencent's "biz message crypt" scheme:
//   - msg_signature is sha1(sorted_join(token, timestamp, nonce, encrypt))
//   - encrypt is AES-256-CBC with key=43-char base64 of aes_key, IV=key[:16]
//   - the cleartext is layout: random(16) | msg_len(4 BE) | msg | corp_id
//
// Reference: https://developer.work.weixin.qq.com/document/path/90968
type wxCrypto struct {
	token  string
	aesKey []byte // 32 bytes, decoded from the 43-char encoded key Tencent gives you
	iv     []byte // first 16 bytes of aesKey
	corpID string
}

// newWxCrypto parses the 43-character base64 EncodingAESKey from the WeChat
// Work admin console. The library appends "=" before decoding because Tencent
// strips the padding character to land at a clean 43 chars.
func newWxCrypto(token, encodedAESKey, corpID string) (*wxCrypto, error) {
	if len(encodedAESKey) != 43 {
		return nil, fmt.Errorf("EncodingAESKey must be 43 chars, got %d", len(encodedAESKey))
	}
	key, err := base64.StdEncoding.DecodeString(encodedAESKey + "=")
	if err != nil {
		return nil, fmt.Errorf("decode EncodingAESKey: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("EncodingAESKey must decode to 32 bytes, got %d", len(key))
	}
	return &wxCrypto{
		token:  token,
		aesKey: key,
		iv:     key[:16],
		corpID: corpID,
	}, nil
}

// signature returns the expected msg_signature for the given message envelope.
// It does NOT compare; callers do their own constant-time check.
func (c *wxCrypto) signature(timestamp, nonce, encrypt string) string {
	parts := []string{c.token, timestamp, nonce, encrypt}
	sort.Strings(parts)
	h := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(h[:])
}

// verifySig is true iff sig matches our computation. We compare hex strings,
// so input case must match (Tencent uses lower-case).
func (c *wxCrypto) verifySig(timestamp, nonce, encrypt, sig string) bool {
	return c.signature(timestamp, nonce, encrypt) == sig
}

// decrypt unwraps a Tencent-encrypted blob and returns the inner message
// bytes. The corp_id suffix in the cleartext is verified against the
// configured corpID — a mismatch means the request was forged or routed to
// the wrong app.
func (c *wxCrypto) decrypt(encryptedB64 string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(encryptedB64)
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("ciphertext is empty or not block-aligned")
	}
	block, err := aes.NewCipher(c.aesKey)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, c.iv).CryptBlocks(plain, data)

	// PKCS#7 unpadding.
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > aes.BlockSize {
		return nil, errors.New("invalid PKCS#7 padding")
	}
	if len(plain) < pad {
		return nil, errors.New("plaintext shorter than padding")
	}
	plain = plain[:len(plain)-pad]

	if len(plain) < 20 {
		return nil, errors.New("plaintext too short for header")
	}
	msgLen := int(binary.BigEndian.Uint32(plain[16:20]))
	if 20+msgLen > len(plain) {
		return nil, fmt.Errorf("declared msg_len %d exceeds plaintext", msgLen)
	}
	msg := plain[20 : 20+msgLen]
	gotCorp := string(plain[20+msgLen:])
	if c.corpID != "" && gotCorp != c.corpID {
		return nil, fmt.Errorf("corp_id mismatch: got %q, want %q", gotCorp, c.corpID)
	}
	return msg, nil
}

// encrypt wraps msg in Tencent's framing and returns the base64 ciphertext.
// Used for echostr replies on URL verification (we don't use it for outbound
// messages — we send those via the normal /cgi-bin/message/send API instead).
func (c *wxCrypto) encrypt(msg []byte) (string, error) {
	var head [16]byte
	if _, err := rand.Read(head[:]); err != nil {
		return "", err
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(msg)))

	plain := make([]byte, 0, 16+4+len(msg)+len(c.corpID))
	plain = append(plain, head[:]...)
	plain = append(plain, lenBuf[:]...)
	plain = append(plain, msg...)
	plain = append(plain, []byte(c.corpID)...)

	// PKCS#7 pad to AES block size. amount is 1..16.
	padN := aes.BlockSize - len(plain)%aes.BlockSize
	for i := 0; i < padN; i++ {
		plain = append(plain, byte(padN))
	}

	block, err := aes.NewCipher(c.aesKey)
	if err != nil {
		return "", err
	}
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, c.iv).CryptBlocks(out, plain)
	return base64.StdEncoding.EncodeToString(out), nil
}
