package stream_test

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/stream"
)

func TestNewSigner(t *testing.T) {
	if stream.KeySize != 32 {
		t.Fatalf("KeySize = %d, want 32", stream.KeySize)
	}
	for _, size := range []int{0, 1, 31, 33, 64} {
		signer, err := stream.NewSigner(make([]byte, size))
		if err == nil || signer != nil {
			t.Fatalf("NewSigner(%d bytes) = %v, %v, want nil signer and error", size, signer, err)
		}
	}
	key := bytes.Repeat([]byte{0x42}, stream.KeySize)
	signer, err := stream.NewSigner(key)
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
	// Independent HMAC-SHA256 vector for domain || binary hash || NUL || path.
	const want = "pAbxLWW5WE5kUNkjbKsoaJkt7UmsOlOSnEg7EiXOaYU"
	if got := signer.Sign(testHash(), path); got != want {
		t.Fatalf("Sign = %q, want %q", got, want)
	}
	mac, err := base64.RawURLEncoding.Strict().DecodeString(want)
	if err != nil || len(mac) != 32 {
		t.Fatalf("signature decoded size = %d, error = %v", len(mac), err)
	}
	if !signer.Verify(testHash(), path, want) {
		t.Fatal("Verify rejected the expected signature")
	}
}

func TestSignerBindsHashAndOriginalPath(t *testing.T) {
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
	otherSigner, err := stream.NewSigner(bytes.Repeat([]byte{0xff}, stream.KeySize))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if otherSigner.Verify(hash, path, signature) {
		t.Fatal("Verify accepted a different signing key")
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
		{name: "standard alphabet plus", sig: "+" + signature[1:]},
		{name: "standard alphabet slash", sig: "/" + signature[1:]},
		{name: "space", sig: " " + signature[1:]},
		{name: "newline", sig: signature[:20] + "\n" + signature[20:]},
		{name: "carriage return", sig: signature[:20] + "\r" + signature[21:]},
		{name: "nonzero padding bits", sig: nonCanonicalSignature(signature)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if signer.Verify(hash, path, tt.sig) {
				t.Fatal("Verify accepted a malformed or tampered signature")
			}
		})
	}
}

func nonCanonicalSignature(signature string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, signature[len(signature)-1])
	return signature[:len(signature)-1] + string(alphabet[last+1])
}

func testSigner(t *testing.T) *stream.Signer {
	t.Helper()
	key := make([]byte, stream.KeySize)
	for i := range key {
		key[i] = byte(i)
	}
	signer, err := stream.NewSigner(key)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return signer
}

func testHash() metainfo.Hash {
	return metainfo.NewHashFromHex("0123456789abcdef0123456789abcdef01234567")
}
