package session

import (
	"errors"
	"strings"
	"testing"

	"github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"
)

// A v2-only source carries only the 32-byte hash, so the v1 field is zero. The
// add must say so instead of claiming the source is missing an info hash.
func TestValidateSourceSpecReportsV2OnlySources(t *testing.T) {
	spec := &torrent.TorrentSpec{}
	spec.InfoHashV2 = generics.Option[infohash_v2.T]{
		Ok:    true,
		Value: infohash_v2.T{0x12, 0x20},
	}

	_, err := validateSourceSpec(spec)
	if !errors.Is(err, ErrV2Only) {
		t.Fatalf("error = %v, want ErrV2Only", err)
	}
	if !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("error = %v, want it to stay ErrInvalidSource for the API", err)
	}
	if !strings.Contains(err.Error(), "v2-only torrents are not supported (no v1 info hash)") {
		t.Fatalf("message = %q, want the v2-only explanation", err.Error())
	}
}

func TestValidateSourceSpecKeepsTheMissingHashError(t *testing.T) {
	_, err := validateSourceSpec(&torrent.TorrentSpec{})
	if err == nil || !strings.Contains(err.Error(), "missing info hash") {
		t.Fatalf("error = %v, want the missing hash message", err)
	}
	if errors.Is(err, ErrV2Only) {
		t.Fatalf("error = %v must not claim v2-only", err)
	}
}

func TestValidateSourceSpecAcceptsV1AndHybrid(t *testing.T) {
	v1 := &torrent.TorrentSpec{InfoHash: [20]byte{0x11}}
	if _, err := validateSourceSpec(v1); err != nil {
		t.Fatalf("v1 spec = %v, want nil", err)
	}

	hybrid := &torrent.TorrentSpec{InfoHash: [20]byte{0x11}}
	hybrid.InfoHashV2 = generics.Option[infohash_v2.T]{Ok: true, Value: infohash_v2.T{0x22}}
	hash, err := validateSourceSpec(hybrid)
	if err != nil {
		t.Fatalf("hybrid spec = %v, want nil", err)
	}
	if hash != [20]byte{0x11} {
		t.Fatalf("hybrid hash = %x, want the v1 hash", hash)
	}
}
