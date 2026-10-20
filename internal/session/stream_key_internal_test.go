package session

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/stream"
)

// A generated capability key must be private and survive a restart unchanged.
func TestStreamKeyPersistsPrivately(t *testing.T) {
	dir := t.TempDir()
	first, err := resolveStreamSigner(dir)
	if err != nil {
		t.Fatalf("resolveStreamSigner: %v", err)
	}
	name := filepath.Join(dir, streamKeyFileName)
	key, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read stream key: %v", err)
	}
	if len(key) != stream.KeySize || bytes.Equal(key, make([]byte, stream.KeySize)) {
		t.Fatalf("generated key has invalid length or all zero bytes")
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatalf("stat stream key: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("stream key permissions = %o, want 600", got)
	}
	second, err := resolveStreamSigner(dir)
	if err != nil {
		t.Fatalf("reload stream key: %v", err)
	}
	var hash metainfo.Hash
	if first.Sign(hash, "Movie.mkv") != second.Sign(hash, "Movie.mkv") {
		t.Fatal("signature changed after reloading the same key")
	}
	stored, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(stored, key) {
		t.Fatalf("key changed on reload: %v", err)
	}
}

// Unsafe or damaged keys fail closed instead of silently revoking old links.
func TestStreamKeyRejectsUnsafeFiles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{name: "short", setup: func(t *testing.T, name string) {
			writeStreamKey(t, name, []byte("short"), 0o600)
		}},
		{name: "long", setup: func(t *testing.T, name string) {
			writeStreamKey(t, name, make([]byte, stream.KeySize+1), 0o600)
		}},
		{name: "public", setup: func(t *testing.T, name string) {
			writeStreamKey(t, name, make([]byte, stream.KeySize), 0o644)
		}},
		{name: "not owner readable", setup: func(t *testing.T, name string) {
			writeStreamKey(t, name, make([]byte, stream.KeySize), 0o200)
		}},
		{name: "directory", setup: func(t *testing.T, name string) {
			if err := os.Mkdir(name, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink", setup: func(t *testing.T, name string) {
			target := filepath.Join(t.TempDir(), "private-key")
			writeStreamKey(t, target, make([]byte, stream.KeySize), 0o600)
			if err := os.Symlink(target, name); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "fifo", setup: func(t *testing.T, name string) {
			if err := syscall.Mkfifo(name, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			name := filepath.Join(dir, streamKeyFileName)
			tc.setup(t, name)
			if _, err := resolveStreamSigner(dir); err == nil || !strings.Contains(err.Error(), "stream key") {
				t.Fatalf("resolveStreamSigner error = %v, want a stream key error", err)
			}
		})
	}
}

func writeStreamKey(t *testing.T, name string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(name, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(name, mode); err != nil {
		t.Fatal(err)
	}
}
