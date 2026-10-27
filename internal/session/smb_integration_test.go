package session_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	client "github.com/hirochachacha/go-smb2"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/session"
	"github.com/yakumioto/torrentfs-go/internal/smb"
)

func smbConfig() config.Config {
	cfg := testConfig()
	cfg.Filesystem.Mode = "smb"
	cfg.SMB.Username, cfg.SMB.Password = "reader", "integration-secret"
	return cfg
}

func serveSMBSession(t *testing.T, cfg config.Config, sess *session.Session) (string, func()) {
	t.Helper()
	srv, err := smb.New(cfg, sess, nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(2 * time.Second):
				t.Error("SMB service failed to drain")
			}
		})
	}
	t.Cleanup(stop)
	return ln.Addr().String(), stop
}

func connectSMBSession(t *testing.T, addr, name string) (*client.Share, func()) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	d := &client.Dialer{Initiator: &client.NTLMInitiator{User: "reader", Password: "integration-secret", Domain: "WORKGROUP"}}
	s, err := d.Dial(conn)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	share, err := s.Mount(name)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	var once sync.Once
	closeClient := func() { once.Do(func() { _ = share.Umount(); _ = s.Logoff(); _ = conn.Close() }) }
	t.Cleanup(closeClient)
	return share, closeClient
}

func TestSMBSessionNamespaceRandomReadsAndRefresh(t *testing.T) {
	cfg := smbConfig()
	sess := newLoopbackSession(t, cfg, filepath.Join(t.TempDir(), "torrents"))
	addr, stop := serveSMBSession(t, cfg, sess)
	share, closeClient := connectSMBSession(t, addr, "torrentfs")
	if entries, err := share.ReadDir(""); err != nil || len(entries) != 0 {
		t.Fatalf("empty library: %v %v", entries, err)
	}
	ctx := context.Background()
	data := make([]byte, 4*testPieceLength+91)
	for i := range data {
		data[i] = byte(i*31 + i/251)
	}
	name := "电影😀.mkv"
	meta, hash := buildSingleFileTorrentBytes(t, name, data, nil)
	addTorrentBytes(t, sess, meta)
	seedPieces(t, sess, hash, data)
	file, err := share.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, off := range []int64{0, testPieceLength - 31, 3*testPieceLength + 7, int64(len(data)) - 73} {
		length := min(8192, int64(len(data))-off)
		got := make([]byte, length)
		if n, err := file.ReadAt(got, off); err != nil || int64(n) != length || !bytes.Equal(got, data[off:off+length]) {
			t.Fatalf("random read at %d: %d %v", off, n, err)
		}
	}
	got, err := io.ReadAll(file)
	if err != nil || sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatalf("full hash: %x %v", sha256.Sum256(got), err)
	}
	t.Logf("SMB Session sha256=%x bytes=%d; cross-piece ranges verified", sha256.Sum256(got), len(got))
	if _, err := sess.CreateCategory(ctx, "Films"); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.CreateCategory(ctx, "Empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.SetTorrentCategory(ctx, hash.HexString(), "Films"); err != nil {
		t.Fatal(err)
	}
	if _, err := share.Stat(name); err == nil {
		t.Fatal("old category path remained visible")
	}
	if _, err := share.Stat("Films/" + name); err != nil {
		t.Fatal(err)
	}
	if entries, err := share.ReadDir("Empty"); err != nil || len(entries) != 0 {
		t.Fatalf("empty category: %v %v", entries, err)
	}
	if n, err := file.ReadAt(make([]byte, 16), 1); n != 16 || err != nil {
		t.Fatalf("existing handle after category move: %d %v", n, err)
	}
	file.Close()

	files := map[string][]byte{"first.bin": bytes.Repeat([]byte("a"), 128<<10), "嵌套😀/second.bin": bytes.Repeat([]byte("b"), 400<<10)}
	for i := range 1600 {
		files[fmt.Sprintf("pages/%04d.txt", i)] = []byte{byte(i)}
	}
	path, multiHash, all := buildMultiFileTorrent(t, t.TempDir(), "Collection", files)
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{MetainfoPath: path}); err != nil {
		t.Fatal(err)
	}
	seedPieces(t, sess, multiHash, all)
	entries, err := share.ReadDir("Collection/pages")
	if err != nil || len(entries) != 1600 {
		t.Fatalf("Session paginated directory: %d %v", len(entries), err)
	}
	for i, e := range entries {
		if e.Name() != fmt.Sprintf("%04d.txt", i) {
			t.Fatalf("page %d: %s", i, e.Name())
		}
	}
	second, err := share.Open("Collection/嵌套😀/second.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(second)
	second.Close()
	if err != nil || sha256.Sum256(got) != sha256.Sum256(files["嵌套😀/second.bin"]) {
		t.Fatalf("cross-file piece read: %v", err)
	}
	if _, err := share.Stat(".metadata"); err == nil {
		t.Fatal("host metadata exposed")
	}
	if _, err := sess.DeleteTorrent(ctx, multiHash.HexString()); err != nil {
		t.Fatal(err)
	}
	if _, err := share.Stat("Collection"); err == nil {
		t.Fatal("deleted torrent remained visible")
	}
	closeClient()
	stop()
}

func TestSMBSubtitleSTRMAndSessionRestart(t *testing.T) {
	cfg := smbConfig()
	cfg.STRM.BaseURL = "http://example.test"
	cfg.SMB.STRMEnabled = true
	dir := filepath.Join(t.TempDir(), "torrents")
	sess := newLoopbackSession(t, cfg, dir)
	data := []byte("movie payload")
	meta, hash := buildSingleFileTorrentBytes(t, "Movie.mkv", data, nil)
	addTorrentBytes(t, sess, meta)
	seedPieces(t, sess, hash, data)
	addr, stop := serveSMBSession(t, cfg, sess)
	share, closeClient := connectSMBSession(t, addr, "torrentfs")
	uploadSubtitle(t, sess, hash, "Movie.mkv", "Movie.srt", "old subtitle")
	old, err := share.Open("Movie.srt")
	if err != nil {
		t.Fatal(err)
	}
	uploadSubtitle(t, sess, hash, "Movie.mkv", "Movie.srt", "new and longer subtitle")
	fresh, err := share.Open("Movie.srt")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		f    *client.File
		want string
	}{{old, "old subtitle"}, {fresh, "new and longer subtitle"}} {
		stat, err := tc.f.Stat()
		if err != nil || stat.Size() != int64(len(tc.want)) {
			t.Fatalf("subtitle stat: %v %v", stat, err)
		}
		got, err := io.ReadAll(tc.f)
		tc.f.Close()
		if err != nil || string(got) != tc.want {
			t.Fatalf("subtitle snapshot: %q %v", got, err)
		}
	}
	strm, closeSTRM := connectSMBSession(t, addr, "torrentfs-strm")
	generated, err := strm.ReadFile("Movie.strm")
	if err != nil {
		t.Fatal(err)
	}
	link, err := sess.StreamURL(hash, "Movie.mkv")
	if err != nil || string(generated) != link+"\n" {
		t.Fatalf("STRM: %q %v", generated, err)
	}
	closeSTRM()
	closeClient()
	stop()
	if err := sess.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored := newLoopbackSession(t, cfg, dir)
	seedPieces(t, restored, hash, data)
	addr, stop = serveSMBSession(t, cfg, restored)
	share, closeClient = connectSMBSession(t, addr, "torrentfs")
	got, err := share.ReadFile("Movie.mkv")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("restart: %q %v", got, err)
	}
	closeClient()
	stop()
}

func TestSMBPeerlessTimeoutAndShutdownCancellation(t *testing.T) {
	for _, timeout := range []time.Duration{100 * time.Millisecond, 10 * time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			cfg := smbConfig()
			cfg.Mount.ReadTimeout = config.Duration(timeout)
			sess := newLoopbackSession(t, cfg, filepath.Join(t.TempDir(), "torrents"))
			data := bytes.Repeat([]byte("peerless"), 128)
			meta, hash := buildSingleFileTorrentBytes(t, "Peerless.mkv", data, nil)
			addTorrentBytes(t, sess, meta)
			addr, stop := serveSMBSession(t, cfg, sess)
			share, closeClient := connectSMBSession(t, addr, "torrentfs")
			file, err := share.Open("Peerless.mkv")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			started := time.Now()
			go func() { _, err := file.ReadAt(make([]byte, 64), 0); done <- err }()
			if timeout > time.Second {
				select {
				case err := <-done:
					t.Fatalf("peerless read returned before shutdown: %v", err)
				case <-time.After(30 * time.Millisecond):
				}
				stop()
			}
			select {
			case err := <-done:
				if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("peerless read masked as EOF: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("peerless read did not terminate")
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("timeout/cancel did not bound read")
			}
			if timeout < time.Second {
				seedPieces(t, sess, hash, data)
				got := make([]byte, 64)
				if _, err := file.ReadAt(got, 0); err != nil || !bytes.Equal(got, data[:64]) {
					t.Fatalf("retry after timeout: %v", err)
				}
			}
			file.Close()
			closeClient()
			stop()
		})
	}
}

func TestSMBDuplicateNamesRemainDistinct(t *testing.T) {
	cfg := smbConfig()
	sess := newLoopbackSession(t, cfg, filepath.Join(t.TempDir(), "torrents"))
	for _, data := range []string{"first", "second"} {
		meta, hash := buildSingleFileTorrentBytes(t, "duplicate.txt", []byte(data), nil)
		addTorrentBytes(t, sess, meta)
		seedPieces(t, sess, hash, []byte(data))
	}
	addr, stop := serveSMBSession(t, cfg, sess)
	share, closeClient := connectSMBSession(t, addr, "torrentfs")
	entries, err := share.ReadDir("")
	if err != nil || len(entries) != 2 {
		t.Fatalf("duplicates: %v %v", entries, err)
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "duplicate.txt") {
			t.Fatal(entry.Name())
		}
		got, err := share.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		seen[string(got)] = true
	}
	if !seen["first"] || !seen["second"] {
		t.Fatalf("colliding files: %v", seen)
	}
	closeClient()
	stop()
}
