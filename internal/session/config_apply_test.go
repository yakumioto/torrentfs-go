package session_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/configctl"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// TestConfigApplierAppliesReadingAllowance covers the hottest of the
// connection keys: a saved reading allowance and read grace reach the running
// allocator through the applier, and the applier leaves the values it does not
// own untouched.
func TestConfigApplierAppliesReadingAllowance(t *testing.T) {
	cfg := testConfig()
	cfg.Connections.ListenHost = "127.0.0.1"
	cfg.Connections.ReadingConns = 3
	cfg.Connections.ReadGrace = config.Duration(time.Minute)
	sess := newLoopbackSession(t, cfg, filepath.Join(t.TempDir(), "torrents"))
	_, advance := fakeConnectionClock(t, sess)

	content := bytes.Repeat([]byte{11}, testPieceLength)
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
	session.AllocateConnectionsForTest(sess)
	if got := session.ConnectionAllowanceForTest(st); got != 3 {
		t.Fatalf("reading allowance = %d, want the configured 3", got)
	}

	if err := handle.(io.Closer).Close(); err != nil {
		t.Fatalf("close the file handle: %v", err)
	}
	advance(time.Minute + time.Second)
	session.AllocateConnectionsForTest(sess)
	if got := session.ConnectionStateForTest(st); got != "idle" {
		t.Fatalf("a torrent past the one-minute read grace is in state %q, want idle", got)
	}
	if got := session.ConnectionAllowanceForTest(st); got != 0 {
		t.Fatalf("a torrent past the read grace has allowance %d, want 0", got)
	}

	applier := session.NewConfigApplier(sess)
	next := cfg
	next.Connections.ReadingConns = 9
	next.Connections.UploadingConns = 7
	next.Connections.ReadGrace = config.Duration(10 * time.Minute)
	// A cold key and a hot key owned by another component: the session
	// applier must adopt neither, because the file holds them but this
	// component never received them.
	next.Cache.CapacityBytes = cfg.Cache.CapacityBytes + 1
	next.HTTP.MaxUploadBytes = cfg.HTTP.MaxUploadBytes + 1
	if err := applier.Apply(next); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	running := sess.Config()
	if running.Connections.ReadingConns != 9 {
		t.Errorf("running reading_conns = %d, want 9", running.Connections.ReadingConns)
	}
	if running.Connections.UploadingConns != 7 {
		t.Errorf("running uploading_conns = %d, want 7", running.Connections.UploadingConns)
	}
	if want := config.Duration(10 * time.Minute); running.Connections.ReadGrace != want {
		t.Errorf("running read_grace = %s, want %s", running.Connections.ReadGrace, want)
	}
	if running.Cache.CapacityBytes != cfg.Cache.CapacityBytes {
		t.Errorf("running cache.capacity = %d, want the unchanged %d",
			running.Cache.CapacityBytes, cfg.Cache.CapacityBytes)
	}
	if running.HTTP.MaxUploadBytes != cfg.HTTP.MaxUploadBytes {
		t.Errorf("running http.max_upload_size = %d, want the unchanged %d",
			running.HTTP.MaxUploadBytes, cfg.HTTP.MaxUploadBytes)
	}

	// The last read happened 61 seconds ago. Under the replaced ten-minute
	// grace the torrent is reading again, with the replaced allowance.
	session.AllocateConnectionsForTest(sess)
	if got := session.ConnectionStateForTest(st); got != "reading" {
		t.Fatalf("state after the replaced read grace = %q, want reading", got)
	}
	if got := session.ConnectionAllowanceForTest(st); got != 9 {
		t.Fatalf("allowance after the replaced reading_conns = %d, want 9", got)
	}
}

// TestConfigApplierAppliesUploadingAllowance covers the uploading allowance:
// a cached torrent with a recent upload gets the configured uploading
// allowance from the replaced snapshot, not from the startup value.
func TestConfigApplierAppliesUploadingAllowance(t *testing.T) {
	cfg := testConfig()
	cfg.Connections.ListenHost = "127.0.0.1"
	cfg.Connections.UploadingConns = 2
	sess := newLoopbackSession(t, cfg, filepath.Join(t.TempDir(), "torrents"))

	now, advance := fakeConnectionClock(t, sess)

	content := bytes.Repeat([]byte{12}, testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	if err := sess.AddTorrent(context.Background(), session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	seedPieces(t, sess, hash, content)
	waitCached(t, context.Background(), st)

	// Nothing is reading the torrent, so the observed upload is what makes it
	// eligible for the uploading allowance.
	session.MarkUploadActivityForTest(st, now())
	advance(2 * time.Minute)
	session.AllocateConnectionsForTest(sess)
	if got := session.ConnectionStateForTest(st); got != "uploading" {
		t.Fatalf("cached torrent with a recent upload is in state %q, want uploading", got)
	}
	if got := session.ConnectionAllowanceForTest(st); got != 2 {
		t.Fatalf("uploading allowance = %d, want the configured 2", got)
	}

	next := cfg
	next.Connections.UploadingConns = 7
	if err := session.NewConfigApplier(sess).Apply(next); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	session.AllocateConnectionsForTest(sess)
	if got := session.ConnectionAllowanceForTest(st); got != 7 {
		t.Fatalf("uploading allowance after the save = %d, want 7", got)
	}
}

// TestConfigApplierConcurrentApplyAndRead drives the applier from one
// goroutine while the snapshot reader and the connection allocator run from
// others. Run under -race, it proves the replacement is synchronized with the
// paths that consume the configuration.
func TestConfigApplierConcurrentApplyAndRead(t *testing.T) {
	cfg := testConfig()
	cfg.Connections.ListenHost = "127.0.0.1"
	cfg.Connections.ReadingConns = 3
	sess := newLoopbackSession(t, cfg, filepath.Join(t.TempDir(), "torrents"))

	content := bytes.Repeat([]byte{13}, testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	if err := sess.AddTorrent(context.Background(), session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	restoreReading := session.MarkReadingForTest(st)
	t.Cleanup(restoreReading)

	low := cfg
	low.Connections.ReadingConns = 2
	low.Connections.ReadGrace = config.Duration(time.Minute)
	high := cfg
	high.Connections.ReadingConns = 9
	high.Connections.UploadingConns = 5
	high.Connections.ReadGrace = config.Duration(5 * time.Minute)
	applier := session.NewConfigApplier(sess)

	const iterations = 100
	var group sync.WaitGroup
	group.Add(3)
	go func() {
		defer group.Done()
		for i := 0; i < iterations; i++ {
			next := low
			if i%2 == 0 {
				next = high
			}
			if err := applier.Apply(next); err != nil {
				t.Errorf("Apply: %v", err)
				return
			}
		}
	}()
	go func() {
		defer group.Done()
		for i := 0; i < iterations; i++ {
			switch got := sess.Config().Connections.ReadingConns; got {
			case 2, 3, 9:
			default:
				t.Errorf("Config() observed reading_conns = %d, want one of the applied snapshots", got)
				return
			}
		}
	}()
	go func() {
		defer group.Done()
		for i := 0; i < iterations; i++ {
			session.AllocateConnectionsForTest(sess)
		}
	}()
	group.Wait()
}

// TestConfigApplierOwnsHotAppliedKeys registers the applier with a real
// controller, which is the same check a start performs: every connection key
// it claims must be marked hot-applied by the schema.
func TestConfigApplierOwnsHotAppliedKeys(t *testing.T) {
	sess := newLoopbackSession(t, testConfig(), filepath.Join(t.TempDir(), "torrents"))
	controller := configctl.New("", config.Default(), nil)
	if err := controller.RegisterApplier(session.NewConfigApplier(sess)); err != nil {
		t.Fatalf("RegisterApplier: %v", err)
	}
}

// TestSaveAppliesConnectionAllowancesAndKeepsColdKeys is the acceptance path
// of the ticket: a save through the configuration controller reaches the
// running allocator, while a cold key keeps its running value and is reported
// for the next restart.
func TestSaveAppliesConnectionAllowancesAndKeepsColdKeys(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(work, "torrentfs.toml")
	body := `[connections]
reading_conns = 3
uploading_conns = 2
read_grace = "1m"

[cache]
capacity = "1GiB"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write configuration file: %v", err)
	}
	cfg, sources, err := config.LoadWithSources(path)
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}
	sess := newLoopbackSession(t, cfg, filepath.Join(work, "torrents"))
	_, advance := fakeConnectionClock(t, sess)

	content := bytes.Repeat([]byte{14}, testPieceLength)
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
	session.AllocateConnectionsForTest(sess)
	if got := session.ConnectionAllowanceForTest(st); got != 3 {
		t.Fatalf("allowance from the file configuration = %d, want 3", got)
	}
	if err := handle.(io.Closer).Close(); err != nil {
		t.Fatalf("close the file handle: %v", err)
	}
	advance(30 * time.Second)

	controller := configctl.New(path, cfg, sources)
	if err := controller.RegisterApplier(session.NewConfigApplier(sess)); err != nil {
		t.Fatalf("RegisterApplier: %v", err)
	}
	view, err := controller.View()
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	result, err := controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: view.Baseline,
		Patch: map[string]string{
			"connections.reading_conns":   "9",
			"connections.uploading_conns": "7",
			"connections.read_grace":      "10m",
		},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	wantApplied := "connections.reading_conns,connections.uploading_conns,connections.read_grace"
	if got := strings.Join(result.Applied, ","); got != wantApplied {
		t.Fatalf("applied = %v, want %s", result.Applied, wantApplied)
	}
	session.AllocateConnectionsForTest(sess)
	if got := session.ConnectionAllowanceForTest(st); got != 9 {
		t.Fatalf("allowance after the save = %d, want 9", got)
	}

	// A cold key is written but not adopted by the running session: the
	// session applier owns the connection keys only.
	view, err = controller.View()
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	result, err = controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: view.Baseline,
		Patch:    map[string]string{"cache.capacity": "2GiB"},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(result.Applied) != 0 {
		t.Errorf("applied = %v, want none for a cold key", result.Applied)
	}
	if got := strings.Join(result.RestartRequired, ","); got != "cache.capacity" {
		t.Errorf("restart required = %v, want [cache.capacity]", result.RestartRequired)
	}
	if got := sess.Config().Cache.CapacityBytes; got != cfg.Cache.CapacityBytes {
		t.Errorf("running cache.capacity = %d, want the unchanged %d", got, cfg.Cache.CapacityBytes)
	}
}
