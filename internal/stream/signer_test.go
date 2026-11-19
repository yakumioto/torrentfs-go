package stream_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/stream"
)

// fixedNow sits inside an expiry quantum so a signature is reproducible.
var fixedNow = time.Date(2026, 11, 19, 10, 15, 0, 0, time.UTC)

const testTTL = 24 * time.Hour

func TestNewSigner(t *testing.T) {
	if stream.KeySize != 32 {
		t.Fatalf("KeySize = %d, want 32", stream.KeySize)
	}
	for _, size := range []int{0, 1, 31, 33, 64} {
		signer, err := stream.NewSigner(make([]byte, size), testTTL)
		if err == nil || signer != nil {
			t.Fatalf("NewSigner(%d bytes) = %v, %v, want nil signer and error", size, signer, err)
		}
	}
	for _, ttl := range []time.Duration{0, -time.Second} {
		signer, err := stream.NewSigner(make([]byte, stream.KeySize), ttl)
		if err == nil || signer != nil {
			t.Fatalf("NewSigner(ttl=%v) = %v, %v, want nil signer and error", ttl, signer, err)
		}
	}
	key := bytes.Repeat([]byte{0x42}, stream.KeySize)
	signer, err := stream.NewSigner(key, testTTL, stream.WithClock(fixedClock))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	want := signer.Sign(testHash(), "Movie.mkv")
	for i := range key {
		key[i] ^= 0xff
	}
	if got := signer.Sign(testHash(), "Movie.mkv"); got != want {
		t.Fatal("mutating the caller's key changed the signer")
	}
}

func TestSignerVector(t *testing.T) {
	signer := testSigner(t)
	path := "目录/Movie 100%#?+.mkv"
	// Independent HMAC-SHA256 vector for domain || binary hash || NUL ||
	// big-endian expiry || NUL || path, prefixed by the expiry it signs.
	const want = "AAAAAGsAGiA3xaH2IOJJK4kZCXGHwnZh-QZy0jiPrNrzvK6PTlknJg"
	signature := signer.Sign(testHash(), path)
	if signature != want {
		t.Fatalf("Sign = %q, want %q", signature, want)
	}
	token, err := base64.RawURLEncoding.Strict().DecodeString(want)
	if err != nil || len(token) != 40 {
		t.Fatalf("signature decoded size = %d, error = %v", len(token), err)
	}
	if !signer.Verify(testHash(), path, want) {
		t.Fatal("Verify rejected the expected signature")
	}
}

func TestSignerBindsHashPathAndExpiry(t *testing.T) {
	signer := testSigner(t)
	hash := testHash()
	const path = "目录/Movie%2F 100%#?+.mkv"
	signature := signer.Sign(hash, path)
	otherHash := hash
	otherHash[0] ^= 1
	for _, tt := range []struct {
		name string
		hash metainfo.Hash
		path string
		want bool
	}{
		{name: "original", hash: hash, path: path, want: true},
		{name: "other hash", hash: otherHash, path: path},
		{name: "other file", hash: hash, path: "目录/Other.mkv"},
		{name: "other directory", hash: hash, path: "其他/Movie%2F 100%#?+.mkv"},
		{name: "changed case", hash: hash, path: "目录/movie%2F 100%#?+.mkv"},
		{name: "decoded percent", hash: hash, path: "目录/Movie/ 100%#?+.mkv"},
		{name: "normalized path", hash: hash, path: "目录/./Movie%2F 100%#?+.mkv"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := signer.Verify(tt.hash, tt.path, signature); got != tt.want {
				t.Fatalf("Verify = %v, want %v", got, tt.want)
			}
		})
	}
	otherSigner := testSignerWithKey(t, bytes.Repeat([]byte{0xff}, stream.KeySize))
	if otherSigner.Verify(hash, path, signature) {
		t.Fatal("Verify accepted a different signing key")
	}
}

func TestSignerExpiresLinks(t *testing.T) {
	signer := testSigner(t)
	hash := testHash()
	const path = "Movie.mkv"
	signature := signer.Sign(hash, path)
	token, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || len(token) != 40 {
		t.Fatalf("decode signature: %v", err)
	}
	expires := time.Unix(int64(binary.BigEndian.Uint64(token[:8])), 0)
	if want := fixedNow.Truncate(time.Hour).Add(testTTL); !expires.Equal(want) {
		t.Fatalf("expiry = %v, want %v", expires, want)
	}

	late := testSignerAt(t, testKey(), expires.Add(30*time.Second))
	if !late.Verify(hash, path, signature) {
		t.Fatal("Verify rejected a link inside the clock skew")
	}
	tooLate := testSignerAt(t, testKey(), expires.Add(2*time.Minute))
	if tooLate.Verify(hash, path, signature) {
		t.Fatal("Verify accepted an expired link")
	}
}

func TestSignerKeepsLinksStableInsideAQuantum(t *testing.T) {
	key := bytes.Repeat([]byte{0x07}, stream.KeySize)
	first, err := stream.NewSigner(key, testTTL, stream.WithClock(fixedClock))
	if err != nil {
		t.Fatal(err)
	}
	later, err := stream.NewSigner(key, testTTL, stream.WithClock(func() time.Time {
		return fixedNow.Add(20 * time.Minute)
	}))
	if err != nil {
		t.Fatal(err)
	}
	if first.Sign(testHash(), "Movie.mkv") != later.Sign(testHash(), "Movie.mkv") {
		t.Fatal("signature changed inside one expiry quantum")
	}
	next, err := stream.NewSigner(key, testTTL, stream.WithClock(func() time.Time {
		return fixedNow.Add(2 * time.Hour)
	}))
	if err != nil {
		t.Fatal(err)
	}
	if first.Sign(testHash(), "Movie.mkv") == next.Sign(testHash(), "Movie.mkv") {
		t.Fatal("signature did not change in the next expiry quantum")
	}
}

func TestSignerRejectsMalformedAndTamperedSignatures(t *testing.T) {
	signer := testSigner(t)
	hash := testHash()
	const path = "Movie.mkv"
	signature := signer.Sign(hash, path)
	tampered := "A" + signature[1:]
	if signature[0] == 'A' {
		tampered = "B" + signature[1:]
	}
	for _, tt := range []struct {
		name string
		sig  string
	}{
		{name: "empty"},
		{name: "tampered", sig: tampered},
		{name: "short", sig: signature[:len(signature)-1]},
		{name: "long", sig: signature + "A"},
		{name: "padded", sig: signature + "="},
		{name: "original shape", sig: base64.RawURLEncoding.EncodeToString(make([]byte, 32))},
		{name: "standard alphabet plus", sig: "+" + signature[1:]},
		{name: "standard alphabet slash", sig: "/" + signature[1:]},
		{name: "space", sig: " " + signature[1:]},
		{name: "newline", sig: signature[:20] + "\n" + signature[20:]},
		{name: "carriage return", sig: signature[:20] + "\r" + signature[21:]},
		{name: "nonzero padding bits", sig: nonCanonicalSignature(t, signature)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if signer.Verify(hash, path, tt.sig) {
				t.Fatal("Verify accepted a malformed or tampered signature")
			}
		})
	}
}

// TestSignerRejectsATamperedExpiry proves the deadline is authenticated: moving
// it forward without the key breaks the HMAC instead of extending the link.
func TestSignerRejectsATamperedExpiry(t *testing.T) {
	signer := testSigner(t)
	hash := testHash()
	const path = "Movie.mkv"
	token, err := base64.RawURLEncoding.DecodeString(signer.Sign(hash, path))
	if err != nil || len(token) != 40 {
		t.Fatalf("decode signature: %v", err)
	}
	token[7] ^= 0x01
	forged := base64.RawURLEncoding.EncodeToString(token)
	if signer.Verify(hash, path, forged) {
		t.Fatal("Verify accepted a forged expiry")
	}
}

func nonCanonicalSignature(t *testing.T, signature string) string {
	t.Helper()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, signature[len(signature)-1])
	if last < 0 || last+1 >= len(alphabet) {
		t.Fatalf("signature %q does not leave a non-canonical replacement", signature)
	}
	return signature[:len(signature)-1] + string(alphabet[last+1])
}

func testSigner(t *testing.T) *stream.Signer {
	t.Helper()
	return testSignerWithKey(t, testKey())
}

func testKey() []byte {
	key := make([]byte, stream.KeySize)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

func testSignerWithKey(t *testing.T, key []byte) *stream.Signer {
	t.Helper()
	signer, err := stream.NewSigner(key, testTTL, stream.WithClock(fixedClock))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return signer
}

func fixedClock() time.Time { return fixedNow }

func testSignerAt(t *testing.T, key []byte, at time.Time) *stream.Signer {
	t.Helper()
	signer, err := stream.NewSigner(key, testTTL, stream.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return signer
}

func testHash() metainfo.Hash {
	return metainfo.NewHashFromHex("0123456789abcdef0123456789abcdef01234567")
}
