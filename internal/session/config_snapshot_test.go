package session_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// TestSessionConfigReturnsStartupSnapshot checks that the session exposes the
// validated configuration it was created with until something replaces it.
func TestSessionConfigReturnsStartupSnapshot(t *testing.T) {
	cfg := testConfig()
	cfg.Connections.ListenHost = "127.0.0.1"
	cfg.Connections.ReadingConns = 7
	sess := newLoopbackSession(t, cfg, filepath.Join(t.TempDir(), "torrents"))

	if got := sess.Config(); !reflect.DeepEqual(got, cfg) {
		t.Fatalf("Config() = %+v, want the startup configuration %+v", got, cfg)
	}
}

// TestSessionSetConfigReplacesSnapshot checks that a replacement becomes the
// current configuration without recreating the session.
func TestSessionSetConfigReplacesSnapshot(t *testing.T) {
	cfg := testConfig()
	cfg.Connections.ListenHost = "127.0.0.1"
	cfg.Connections.ReadingConns = 7
	sess := newLoopbackSession(t, cfg, filepath.Join(t.TempDir(), "torrents"))

	next := cfg
	next.Connections.ReadingConns = 11
	next.Proxy.Socks5URL = "socks5://proxy.example:1080"
	sess.SetConfig(next)

	if got := sess.Config(); !reflect.DeepEqual(got, next) {
		t.Fatalf("Config() after SetConfig = %+v, want %+v", got, next)
	}
}

// TestSessionConfigCopiesDoNotAliasTheSnapshot checks that neither the value
// returned by Config nor the value passed to SetConfig can mutate the session
// through a shared slice.
func TestSessionConfigCopiesDoNotAliasTheSnapshot(t *testing.T) {
	cfg := testConfig()
	cfg.Connections.ListenHost = "127.0.0.1"
	cfg.Connections.BootstrapNodes = []string{"router.example:6881"}
	sess := newLoopbackSession(t, cfg, filepath.Join(t.TempDir(), "torrents"))

	got := sess.Config()
	got.Connections.BootstrapNodes[0] = "mutated.example:6881"
	if nodes := sess.Config().Connections.BootstrapNodes; nodes[0] != "router.example:6881" {
		t.Fatalf("mutating a Config() copy changed the snapshot: %v", nodes)
	}

	next := cfg
	next.Connections.BootstrapNodes = []string{"other.example:6881"}
	sess.SetConfig(next)
	next.Connections.BootstrapNodes[0] = "mutated.example:6881"
	if nodes := sess.Config().Connections.BootstrapNodes; nodes[0] != "other.example:6881" {
		t.Fatalf("mutating the SetConfig argument changed the snapshot: %v", nodes)
	}
}

// TestConnectionAllocatorUsesReplacedConfig checks that a replacement reaches
// the connection allocator without recreating the session: the same open file
// handle gets the allowance from the current snapshot.
func TestConnectionAllocatorUsesReplacedConfig(t *testing.T) {
	cfg := testConfig()
	cfg.Connections.ListenHost = "127.0.0.1"
	cfg.Connections.ReadingConns = 5
	sess := newLoopbackSession(t, cfg, filepath.Join(t.TempDir(), "torrents"))

	content := bytes.Repeat([]byte{1}, testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	if err := sess.AddTorrent(context.Background(), session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	handle, err := sess.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = handle.(io.Closer).Close() }()

	session.AllocateConnectionsForTest(sess)
	if got := session.ConnectionAllowanceForTest(st); got != 5 {
		t.Fatalf("reading allowance with the startup config = %d, want 5", got)
	}

	next := cfg
	next.Connections.ReadingConns = 9
	sess.SetConfig(next)
	session.AllocateConnectionsForTest(sess)
	if got := session.ConnectionAllowanceForTest(st); got != 9 {
		t.Fatalf("reading allowance after SetConfig = %d, want 9", got)
	}
}

// TestOpenFileUsesReplacedReadTimeout checks that the read timeout is taken
// from the current snapshot when a file is opened, not from the startup copy.
func TestOpenFileUsesReplacedReadTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.Connections.ListenHost = "127.0.0.1"
	cfg.Filesystem.ReadTimeout = config.Duration(20 * time.Second)
	sess := newLoopbackSession(t, cfg, filepath.Join(t.TempDir(), "torrents"))

	content := bytes.Repeat([]byte{2}, testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	if err := sess.AddTorrent(context.Background(), session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}

	next := cfg
	next.Filesystem.ReadTimeout = config.Duration(150 * time.Millisecond)
	sess.SetConfig(next)

	handle, err := sess.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = handle.(io.Closer).Close() }()

	// The torrent has metadata but no source, so the read waits for its
	// timeout. The startup value (20s) would outlive this test.
	started := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := handle.ReadAt(make([]byte, 64), 0)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("peerless read error = %v, want DeadlineExceeded", err)
		}
		if elapsed := time.Since(started); elapsed < 100*time.Millisecond {
			t.Fatalf("peerless read returned after %s, before the replaced 150ms timeout", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("peerless read did not use the replaced timeout")
	}
}
