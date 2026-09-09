package session

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/yakumioto/torrentfs-go/internal/cache"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

const internalTestPieceLength = 256 << 10

func internalTestConfig(dataDir string) config.Config {
	cfg := config.Default()
	cfg.Paths.DataDir = dataDir
	return cfg
}

func TestSessionPieceCacheHitCountAndCloseInvalidation(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := bytes.Repeat([]byte("cache"), internalTestPieceLength/5+1)
	torrentPath, hash := buildInternalTestTorrent(t, dataDir, work, content)

	sess, err := New(internalTestConfig(dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sess.AddTorrent(ctx, Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	waitInternalTorrentComplete(t, ctx, st)

	ra, err := sess.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	piece := make([]byte, internalTestPieceLength)
	if _, err := ra.ReadAt(piece, 0); err != nil {
		t.Fatalf("first piece read: %v", err)
	}
	key := cache.Key{Torrent: hash.HexString(), Piece: 0}
	if !sess.pieceCache.Has(key) {
		t.Fatal("first piece was not cached")
	}
	before := sess.pieceCache.HitCount()
	if _, err := ra.ReadAt(make([]byte, 32), 1); err != nil {
		t.Fatalf("cached piece read: %v", err)
	}
	if got := sess.pieceCache.HitCount(); got <= before {
		t.Fatalf("HitCount = %d after cached read, want > %d", got, before)
	}

	start := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 32; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			_, _ = ra.ReadAt(make([]byte, 64), 0)
		}()
	}
	close(start)
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	readers.Wait()
	if got := sess.pieceCache.Len(); got != 0 {
		t.Fatalf("cache Len after concurrent close = %d, want 0", got)
	}
}

func TestSessionUsesConfiguredPieceCacheCapacity(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := internalTestConfig(dataDir)
	cfg.Cache.CapacityBytes = 1234

	sess, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if got := sess.pieceCache.Capacity(); got != 1234 {
		t.Fatalf("piece cache capacity = %d, want 1234", got)
	}
}

func TestRaFileMissCannotPutAfterCloseAndInvalidate(t *testing.T) {
	store := cache.New(64)
	source := &gatedPieceSource{
		started: make(chan struct{}),
		release: make(chan struct{}),
		data:    []byte("piece"),
	}
	file := &raFile{
		loader:      source,
		cache:       store,
		torrentKey:  "old-hash",
		fileSize:    int64(len(source.data)),
		pieceLength: int64(len(source.data)),
		torrentSize: int64(len(source.data)),
	}
	result := make(chan error, 1)
	go func() {
		_, err := file.ReadAt(make([]byte, len(source.data)), 0)
		result <- err
	}()
	<-source.started
	if err := file.Close(); err != nil {
		t.Fatalf("raFile.Close: %v", err)
	}
	store.InvalidateTorrent("old-hash")
	close(source.release)
	if err := <-result; !errors.Is(err, filesystem.ErrClosed) {
		t.Fatalf("ReadAt error = %v, want ErrClosed", err)
	}
	if got := store.Len(); got != 0 {
		t.Fatalf("cache Len after close/invalidate race = %d, want 0", got)
	}
}

type gatedPieceSource struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	data    []byte
}

func (s *gatedPieceSource) prepare(*torrent.Torrent, int, int, int64) error {
	return nil
}

func (s *gatedPieceSource) ReadAt(dst []byte, off int64) (int, error) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	if off < 0 || off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(dst, s.data[off:])
	return n, nil
}

func (s *gatedPieceSource) Close() error {
	return nil
}

type unstablePieceStateSource struct {
	calls int
}

func (s *unstablePieceStateSource) PieceStateRuns() torrent.PieceStateRuns {
	return torrent.PieceStateRuns{{
		PieceState: torrent.PieceState{
			Completion: storage.Completion{Ok: true},
			Partial:    true,
		},
		Length: 1,
	}}
}

func (s *unstablePieceStateSource) PieceBytesMissing(int) int64 {
	s.calls++
	return int64(s.calls * 10)
}

func (s *unstablePieceStateSource) Length() int64 {
	return 100
}

func TestPieceStatesRejectsUnstableSnapshot(t *testing.T) {
	source := &unstablePieceStateSource{}
	states, err := pieceStatesSnapshot(source, &metainfo.Info{PieceLength: 100})
	if !errors.Is(err, errPieceStateSnapshotUnstable) {
		t.Fatalf("pieceStatesSnapshot error = %v, want unstable snapshot error", err)
	}
	if states != nil {
		t.Fatalf("pieceStatesSnapshot states = %+v, want nil", states)
	}
}

func TestSessionLargePieceReadBypassesCache(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := []byte("small request from a large piece")
	torrentPath, hash := buildInternalTestTorrentWithPieceLength(t, dataDir, work, content, config.Default().Cache.CapacityBytes+1)

	sess, err := New(internalTestConfig(dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sess.AddTorrent(ctx, Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	waitInternalTorrentComplete(t, ctx, st)

	ra, err := sess.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	got := make([]byte, len(content))
	if n, err := ra.ReadAt(got, 0); err != nil || n != len(content) {
		t.Fatalf("large-piece read = %d bytes, %v; want %d, nil", n, err, len(content))
	}
	if string(got) != string(content) {
		t.Fatalf("large-piece content = %q, want %q", got, content)
	}
	if got := sess.pieceCache.Len(); got != 0 {
		t.Fatalf("cache Len after oversized piece read = %d, want 0", got)
	}
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func buildInternalTestTorrent(t *testing.T, dataDir, torrentDir string, content []byte) (string, metainfo.Hash) {
	return buildInternalTestTorrentWithPieceLength(t, dataDir, torrentDir, content, internalTestPieceLength)
}

func buildInternalTestTorrentWithPieceLength(t *testing.T, dataDir, torrentDir string, content []byte, pieceLength int64) (string, metainfo.Hash) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("make data dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "payload.bin"), content, 0o644); err != nil {
		t.Fatalf("write data: %v", err)
	}
	pieceSize := int(pieceLength)
	pieces := make([]byte, 0, (len(content)+pieceSize-1)/pieceSize*sha1.Size)
	for off := 0; off < len(content); off += pieceSize {
		end := off + pieceSize
		if end > len(content) {
			end = len(content)
		}
		sum := sha1.Sum(content[off:end])
		pieces = append(pieces, sum[:]...)
	}
	info := metainfo.Info{
		Name:        "payload.bin",
		Length:      int64(len(content)),
		PieceLength: pieceLength,
		Pieces:      pieces,
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("encode info: %v", err)
	}
	mi := metainfo.MetaInfo{InfoBytes: bencode.Bytes(infoBytes)}
	encoded, err := bencode.Marshal(mi)
	if err != nil {
		t.Fatalf("encode metainfo: %v", err)
	}
	path := filepath.Join(torrentDir, "payload.bin.torrent")
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatalf("write torrent: %v", err)
	}
	return path, mi.HashInfoBytes()
}

func waitInternalTorrentComplete(t *testing.T, ctx context.Context, st *Torrent) {
	t.Helper()
	select {
	case <-st.GotInfo():
	case <-ctx.Done():
		t.Fatalf("timed out waiting for torrent info: %v", ctx.Err())
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if st.BytesCompleted() == st.Length() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("torrent never became complete: %d/%d", st.BytesCompleted(), st.Length())
}
