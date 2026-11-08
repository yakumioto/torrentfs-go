package session_test

import (
	"path/filepath"
	"reflect"
	"testing"
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
