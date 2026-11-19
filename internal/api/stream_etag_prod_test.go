package api

import (
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

// TestStreamETagIsPathDerived pins the production derivation so the external
// tests' expectation cannot drift away from it unnoticed: the validator depends
// on the torrent and the file path only, never on the signing capability.
func TestStreamETagIsPathDerived(t *testing.T) {
	var hash metainfo.Hash
	for i := range hash {
		hash[i] = byte(i)
	}
	const path = "Season 1/第一集.mkv"

	sum := sha256.Sum256([]byte(hash.HexString() + "\x00" + path))
	want := fmt.Sprintf("%q", fmt.Sprintf("%x", sum[:8]))
	if got := streamETag(hash, path); got != want {
		t.Fatalf("streamETag = %q, want %q", got, want)
	}

	var other metainfo.Hash
	other[0] = 0xff
	if streamETag(other, path) == want {
		t.Fatal("two torrents produced the same validator for the same path")
	}
	if streamETag(hash, path+"x") == want {
		t.Fatal("two paths produced the same validator for the same torrent")
	}
}
