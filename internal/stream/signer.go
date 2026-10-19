package stream

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"github.com/anacrolix/torrent/metainfo"
)

// KeySize is the exact byte length of a stream signing key.
const KeySize = 32

// The versioned domain is terminated before the fixed-size binary infohash.
const signatureDomain = "torrentfs-stream-v1\x00"

// Signer authenticates original torrent paths without retaining the caller's key.
// Its methods are safe for concurrent use.
type Signer struct {
	key [KeySize]byte
}

// NewSigner copies an exactly KeySize-byte key into a new signer.
func NewSigner(key []byte) (*Signer, error) {
	if len(key) != KeySize {
		return nil, errors.New("stream: signing key must be exactly 32 bytes")
	}
	s := &Signer{}
	copy(s.key[:], key)
	return s, nil
}

// Sign binds the signature to the hash and the original, unnormalized path.
func (s *Signer) Sign(hash metainfo.Hash, path string) string {
	return base64.RawURLEncoding.EncodeToString(s.sum(hash, path))
}

// Verify checks a canonical unpadded base64url signature using constant-time
// comparison of the full HMAC-SHA256 value.
func (s *Signer) Verify(hash metainfo.Hash, path, signature string) bool {
	mac, ok := decodeSignature(signature)
	return ok && hmac.Equal(mac, s.sum(hash, path))
}

func (s *Signer) sum(hash metainfo.Hash, path string) []byte {
	mac := hmac.New(sha256.New, s.key[:])
	_, _ = mac.Write([]byte(signatureDomain))
	_, _ = mac.Write(hash[:])
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(path))
	return mac.Sum(nil)
}

func decodeSignature(signature string) ([]byte, bool) {
	if len(signature) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return nil, false
	}
	for i := range len(signature) {
		c := signature[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_') {
			return nil, false
		}
	}
	mac, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	return mac, err == nil && len(mac) == sha256.Size
}
