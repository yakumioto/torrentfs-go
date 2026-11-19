package stream

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

// KeySize is the exact byte length of a stream signing key.
const KeySize = 32

// The versioned domain is terminated before the fixed-size binary infohash.
// The v2 domain binds an expiry into every signature: a link is a credential
// with a deadline, not one that stays valid forever.
const signatureDomain = "torrentfs-stream-v2\x00"

const (
	// expiryQuantum rounds the expiry down so every signing call inside the
	// same quantum produces the same link. Without it the .strm text would
	// change on each read, and its size is what the virtual file reports.
	expiryQuantum = time.Hour
	// clockSkew is how far a verifier's clock may run past an expiry and still
	// accept the link. It absorbs a client or container clock that is a little
	// ahead of the signer's.
	clockSkew = time.Minute
	// tokenSize is the expiry followed by the HMAC-SHA256 value.
	tokenSize = 8 + sha256.Size
)

// Option configures a Signer.
type Option func(*Signer)

// WithClock replaces the signer's clock. Tests use it to pin expiry.
func WithClock(now func() time.Time) Option {
	return func(s *Signer) {
		if now != nil {
			s.now = now
		}
	}
}

// Signer authenticates original torrent paths without retaining the caller's key.
// Its methods are safe for concurrent use.
type Signer struct {
	key [KeySize]byte
	ttl time.Duration
	now func() time.Time
}

// NewSigner copies an exactly KeySize-byte key into a new signer. Every
// signature it produces expires ttl after the current expiry quantum.
func NewSigner(key []byte, ttl time.Duration, opts ...Option) (*Signer, error) {
	if len(key) != KeySize {
		return nil, errors.New("stream: signing key must be exactly 32 bytes")
	}
	if ttl <= 0 {
		return nil, errors.New("stream: link lifetime must be positive")
	}
	s := &Signer{ttl: ttl, now: time.Now}
	copy(s.key[:], key)
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s, nil
}

// Sign binds the signature to the hash, the original unnormalized path, and an
// expiry. The expiry travels inside the token, so a copied link dies on its own.
func (s *Signer) Sign(hash metainfo.Hash, path string) string {
	expires := s.now().Truncate(expiryQuantum).Add(s.ttl).Unix()
	token := make([]byte, tokenSize)
	binary.BigEndian.PutUint64(token, uint64(expires))
	copy(token[8:], s.sum(hash, path, expires))
	return base64.RawURLEncoding.EncodeToString(token)
}

// expiry reports when a verifiable signature stops being accepted. It returns
// false for a malformed, expired, or tampered signature.
func (s *Signer) expiry(hash metainfo.Hash, path, signature string) (time.Time, bool) {
	token, ok := decodeSignature(signature)
	if !ok {
		return time.Time{}, false
	}
	expires := int64(binary.BigEndian.Uint64(token[:8]))
	at := time.Unix(expires, 0)
	if s.now().After(at.Add(clockSkew)) {
		return time.Time{}, false
	}
	if !hmac.Equal(token[8:], s.sum(hash, path, expires)) {
		return time.Time{}, false
	}
	return at, true
}

// Verify checks a canonical unpadded base64url signature using constant-time
// comparison of the full HMAC-SHA256 value, then enforces its expiry.
func (s *Signer) Verify(hash metainfo.Hash, path, signature string) bool {
	_, ok := s.expiry(hash, path, signature)
	return ok
}

func (s *Signer) sum(hash metainfo.Hash, path string, expires int64) []byte {
	mac := hmac.New(sha256.New, s.key[:])
	var stamp [8]byte
	binary.BigEndian.PutUint64(stamp[:], uint64(expires))
	_, _ = mac.Write([]byte(signatureDomain))
	_, _ = mac.Write(hash[:])
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(stamp[:])
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(path))
	return mac.Sum(nil)
}

func decodeSignature(signature string) ([]byte, bool) {
	if len(signature) != base64.RawURLEncoding.EncodedLen(tokenSize) {
		return nil, false
	}
	for i := range len(signature) {
		c := signature[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_') {
			return nil, false
		}
	}
	token, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	return token, err == nil && len(token) == tokenSize
}
